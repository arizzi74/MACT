package releasemanager

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Go's asynchronous preemption can interrupt poll with SIGURG. Retry without
// resetting the deadline, just as the production terminal reader retries EINTR.
func setupTestPoll(fds []unix.PollFd, timeout int) (int, error) {
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, nil
		}
		ready, err := unix.Poll(fds, int((remaining+time.Millisecond-1)/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return ready, err
	}
}

func setupTestTerminal(t *testing.T) (*os.File, *workerSetupTerminal) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skip("pseudo terminals are not available")
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	state, err := workerTerminalState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	state.Lflag |= unix.ECHO | unix.ICANON
	if err := setWorkerTerminalState(int(slave.Fd()), state); err != nil {
		t.Fatal(err)
	}
	return master, &workerSetupTerminal{file: slave}
}

func TestSetupTerminalShowsEnrollmentURLWithoutChangingSettings(t *testing.T) {
	master, terminal := setupTestTerminal(t)
	before, err := workerTerminalState(int(terminal.file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := terminal.Ask(ctx, "Enrollment URL", "", false)
		done <- result{answer, err}
	}()
	poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
	if ready, err := setupTestPoll(poll, 2000); err != nil || ready == 0 {
		t.Fatal("enrollment URL prompt did not appear", err)
	}
	during, err := workerTerminalState(int(terminal.file.Fd()))
	if err != nil || *before != *during {
		t.Fatal("visible enrollment prompt changed terminal settings", err)
	}
	const enrollmentURL = "https://gateway.example/tgw/enroll/#ABCD2345EFGH"
	if _, err := unix.Write(int(master.Fd()), []byte(enrollmentURL+"\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.answer != enrollmentURL {
			t.Fatalf("visible prompt returned %q, %v", result.answer, result.err)
		}
	case <-ctx.Done():
		t.Fatal("visible enrollment prompt did not finish", ctx.Err())
	}
	after, err := workerTerminalState(int(terminal.file.Fd()))
	if err != nil || *before != *after {
		t.Fatal("visible enrollment prompt changed terminal settings", err)
	}
	var output strings.Builder
	for {
		ready, err := setupTestPoll(poll, 20)
		if err != nil {
			t.Fatal(err)
		}
		if ready == 0 {
			break
		}
		buffer := make([]byte, 1024)
		n, err := unix.Read(int(master.Fd()), buffer)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(buffer[:n])
	}
	if !strings.Contains(output.String(), "Enrollment URL: "+enrollmentURL) {
		t.Fatal("terminal did not echo the complete enrollment URL")
	}
}

// A PTY master write queues input for asynchronous delivery. Wait for the
// slave's line discipline to process flow-control bytes after this short input
// before cancelling, so the test exercises flushing received canonical input.
func setupTestWritePendingInput(t *testing.T, master, slave *os.File, input string) string {
	t.Helper()
	state, err := workerTerminalState(int(slave.Fd()))
	if err != nil || state.Iflag&unix.IXON == 0 || state.Cc[unix.VSTOP] == 0 || state.Cc[unix.VSTART] == 0 {
		t.Fatal("PTY flow control is unavailable", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCPKT, 1); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCPKT, 0); err != nil {
			t.Error(err)
		}
	}()
	bytes := append([]byte(input), state.Cc[unix.VSTOP], state.Cc[unix.VSTART])
	if n, err := unix.Write(int(master.Fd()), bytes); err != nil || n != len(bytes) {
		t.Fatal("could not write pending terminal input", err)
	}
	var output strings.Builder
	poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := int(time.Until(deadline) / time.Millisecond)
		if remaining <= 0 {
			t.Fatal("PTY did not acknowledge pending terminal input")
		}
		if ready, err := setupTestPoll(poll, remaining); err != nil || ready == 0 {
			t.Fatal("PTY did not acknowledge pending terminal input", err)
		}
		var packet [1024]byte
		n, err := unix.Read(int(master.Fd()), packet[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || n == 0 {
			t.Fatal("could not read PTY input acknowledgment", err)
		}
		if packet[0] == unix.TIOCPKT_DATA {
			output.Write(packet[1:n])
		} else if packet[0]&unix.TIOCPKT_START != 0 {
			return output.String()
		}
	}
}

func TestSetupTerminalHidesTokenAndRestoresEcho(t *testing.T) {
	for _, cancelInput := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "cancel"}[cancelInput], func(t *testing.T) {
			master, terminal := setupTestTerminal(t)
			before, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				answer string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				answer, err := terminal.Ask(ctx, "Enrollment token", "", true)
				done <- result{answer, err}
			}()
			poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
			if ready, err := setupTestPoll(poll, 2000); err != nil || ready == 0 {
				t.Fatal("secret prompt did not appear", err)
			}
			state, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil || state.Lflag&unix.ECHO != 0 {
				t.Fatal("echo was not disabled before displaying the token prompt", err)
			}
			const token = "test-secret-never-echo"
			var output strings.Builder
			if cancelInput {
				output.WriteString(setupTestWritePendingInput(t, master, terminal.file, token))
				cancel()
			} else if _, err := unix.Write(int(master.Fd()), []byte(token+"\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				if cancelInput {
					if !errors.Is(result.err, context.Canceled) || result.answer != "" {
						t.Fatal("cancelled prompt did not stop cleanly", result.err)
					}
				} else if result.err != nil || result.answer != token {
					t.Fatal("secret prompt did not read the token", result.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("terminal prompt did not finish")
			}
			after, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil || *before != *after {
				t.Fatal("terminal settings were not restored", err)
			}
			if cancelInput {
				// After cancellation, a newline must not expose the partially typed
				// secret to the next program that reads this terminal.
				if _, err := unix.Write(int(master.Fd()), []byte("\n")); err != nil {
					t.Fatal(err)
				}
				input := []unix.PollFd{{Fd: int32(terminal.file.Fd()), Events: unix.POLLIN}}
				if ready, err := setupTestPoll(input, 1000); err != nil || ready == 0 {
					t.Fatal("terminal input unavailable after cancellation", err)
				}
				buffer := make([]byte, 128)
				n, err := unix.Read(int(terminal.file.Fd()), buffer)
				if err != nil || string(buffer[:n]) != "\n" {
					t.Fatal("partial enrollment token was left in terminal input", err)
				}
			}
			for {
				ready, err := setupTestPoll(poll, 20)
				if err != nil {
					t.Fatal(err)
				}
				if ready == 0 {
					break
				}
				buffer := make([]byte, 1024)
				n, err := unix.Read(int(master.Fd()), buffer)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(buffer[:n])
			}
			if strings.Contains(output.String(), token) {
				t.Fatal("terminal echoed the enrollment token")
			}
		})
	}
}

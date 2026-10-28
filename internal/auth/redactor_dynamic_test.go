package auth

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRedactorConcurrentCredentialRotationPreservesPriorValues(t *testing.T) {
	r, err := NewRedactor([]string{"startup-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 200; iteration++ {
				if got := r.Redact("startup-secret"); got != "[REDACTED]" {
					t.Errorf("startup credential exposed: %q", got)
				}
			}
		}()
	}
	for writer := 0; writer < 6; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			for rotation := 0; rotation < 12; rotation++ {
				secret := fmt.Sprintf("credential.$(%d)[%d].end", writer, rotation)
				r.AddLiteral(secret)
				r.AddLiteral(secret)
				if got := r.Redact("prefix " + secret + " suffix"); strings.Contains(got, secret) {
					t.Errorf("new credential exposed: %q", got)
				}
			}
		}(writer)
	}
	group.Wait()
	for writer := 0; writer < 6; writer++ {
		for rotation := 0; rotation < 12; rotation++ {
			secret := fmt.Sprintf("credential.$(%d)[%d].end", writer, rotation)
			if got := r.Redact(secret); got != "[REDACTED]" {
				t.Errorf("rotated credential lost: %q", got)
			}
		}
	}
	if len(r.literals) != 72 {
		t.Fatalf("duplicate literal rules retained: %d", len(r.literals))
	}
}

# Local Piper Miro and Ryan speech synthesis

`codex-piper` keeps one Sherpa-ONNX **v1.13.8** CPU engine and one resampler
resident and accepts sequential requests over stdin/stdout. It performs native
CPU inference without Python, FFmpeg, a server, system eSpeak, or system ONNX
Runtime. The release includes `libsherpa-onnx-c-api` and `libonnxruntime`; Linux
uses glibc and the normal C++ runtime, and macOS uses Apple system libraries.
The Linux helper is built on Ubuntu 22.04; macOS targets macOS 13 or later.

Italian selects the male **Miro** voice in
`ENGINE_DIR/vits-piper-it_IT-miro-high/`, containing `it_IT-miro-high.onnx`.
English selects the U.S. English **Ryan** voice in
`ENGINE_DIR/vits-piper-en_US-ryan-medium/`, containing `en_US-ryan-medium.onnx`.
Each directory also contains its own `tokens.txt` and `espeak-ng-data/`.
Both model waveforms are 22050 Hz and are converted to 24000 Hz with Sherpa/Kaldi's
windowed-sinc low-pass resampler; each request resets and flushes the resampler.
Protocol 1 announces
`{"type":"ready","protocol":1,"sample_rate":24000,"language":"english"}`
(or `"italian"` for Miro),
reads a bounded JSON line containing `id` and `text`, then returns a JSON header
and mono float32 little-endian PCM. Text is limited to 32 KiB, JSON headers to
64 KiB, output to 600 seconds, and CPU threads to 1–32. Invalid framing closes
the process; a failed generation returns `synthesis_failed`. Neither request
text nor audio is stored on disk. EOF releases the resident engine.

## Runtime source and licenses

The exact upstream source and all four official CPU runtime archive SHA256s
are pinned in `source.env`, verified before extraction, and packaged alongside
the executable. The runtime uses Sherpa's Apache 2.0 code (Copyright Xiaomi
Corporation and contributors) and ONNX Runtime
**1.28.2** under MIT, including its bundled third-party notices. Sherpa's VITS
phonemization includes **eSpeak NG under GPL 3.0**, plus Piper phonemize under
MIT. The presence of these libraries does not make the Miro weights permissive.
The relevant license texts are included in `licenses/`.

Corresponding runtime sources and dependency build instructions:

- Sherpa: https://github.com/k2-fsa/sherpa-onnx/tree/v1.13.8
- ONNX Runtime: https://github.com/microsoft/onnxruntime/tree/v1.28.2
- eSpeak NG: https://github.com/csukuangfj/espeak-ng/tree/ed530aa113046142eb5115cf2fc9157854d0ffe1
- Piper phonemize: https://github.com/csukuangfj/piper-phonemize/tree/f3ff95afc03640bc1399e113e83361192a2fafb4
- Kaldi native fbank 1.22.3: https://github.com/csukuangfj/kaldi-native-fbank/tree/v1.22.3
- Kaldi decoder 0.3.0: https://github.com/k2-fsa/kaldi-decoder/tree/v0.3.0
- Kaldifst 1.8.0: https://github.com/k2-fsa/kaldifst/tree/v1.8.0
- KissFFT: https://github.com/mborgerding/kissfft/tree/febd4caeed32e33ad8b2e0bb5ea77542c40f18ec
- OpenFst: https://github.com/csukuangfj/openfst/tree/v1.8.5-2026-07-09
- simple-sentencepiece: https://github.com/pkufool/simple-sentencepiece/tree/v0.7
- nlohmann JSON: https://github.com/nlohmann/json/tree/v3.12.0
- hclust-cpp: https://github.com/csukuangfj/hclust-cpp/tree/2026-02-25

## Miro attribution and restrictions

The Miro voice, training data, and model belong to **TigreGotico Lda**,
https://tigregotico.pt. The current upstream model card identifies
**Creative Commons Attribution-NonCommercial-NoDerivatives 4.0 (CC BY-NC-ND 4.0)**:
https://huggingface.co/OpenVoiceOS/pipertts_it-IT_miro/blob/687b8fff3ba49a9b5b6df843e2bbf1b7855a5260/README.md
Its training-data attribution is
https://huggingface.co/datasets/TigreGotico/tts-train-synthetic-miro_it-IT.

The pinned Sherpa model archive contains an older README declaring
**CC BY-NC-SA 4.0**. Both that README and the current upstream card are
preserved in `licenses/`, with both Creative Commons license texts. This
discrepancy is recorded explicitly; no general commercial permission is
asserted. For commercial use, derivative models, or licensing the Miro voice
identity, contact TigreGotico Lda.

## Ryan attribution and restrictions

The pinned Ryan medium model archive comes from the official Sherpa-ONNX
`tts-models` release:
https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-en_US-ryan-medium.tar.bz2.
Its `MODEL_CARD` describes a single-speaker U.S. English model fine-tuned from
the U.S. English Lessac medium voice. The card identifies the **RyanSpeech**
dataset at https://www.kaggle.com/datasets/roholazandie/ryanspeech and lists
**Creative Commons Attribution-NonCommercial-ShareAlike 4.0 (CC BY-NC-SA 4.0)**.
The exact archived card is preserved as `licenses/Ryan-archive-MODEL_CARD.md`;
its SHA256, the archive SHA256, and the ONNX SHA256 are pinned in `source.env`.
The CC BY-NC-SA license text is included in `licenses/`. No general commercial
permission for this voice is asserted.

Both voices' model weights are downloaded separately and are not bundled in
the helper release archives. Each extracted model has a separate verified
inventory; installing Ryan leaves a valid Miro installation in place.

Build with `scripts/build-piper-linux.sh amd64|arm64` on Linux (Docker), or
`scripts/build-piper.sh` on a matching Mac. `PIPER_OUTPUT_DIR` defaults to
`dist/piper`; the result is `codex-piper-OS-ARCH.tar.gz`, rooted at the helper,
`lib/`, these notices, `licenses/`, `source.env`, and `SHA256SUMS`. An extracted
package can be moved as a unit. `PIPER_SOURCE_ARCHIVE` and
`PIPER_RUNTIME_ARCHIVE` permit verified offline build inputs. Set
`PIPER_MODEL_DIR` to run real two-request smoke checks in both language modes
after compilation; this build-time check uses Go, with no audio files.

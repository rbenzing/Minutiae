# Minutiae

Forensic acquisition and (planned) analysis for mobile devices — finding the
smallest details, including hidden and deleted data.

Use only on devices you are authorized to examine.

## Build
```bash
go run ./tools/check      # full verification
go build -o bin/minutiae.exe ./cmd/minutiae
```

## Quick start
```bash
minutiae case new --dir ./cases --id CASE01 --examiner "Jane Doe"
minutiae devices
minutiae case verify --case ./cases/CASE01
```

Roadmap: [docs/ROADMAP.md](docs/ROADMAP.md).

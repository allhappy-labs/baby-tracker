# Ask answer repair verification — 2026-09-29

The Ask-only repair was tested and fast-forwarded to local `main` at `b2a13f7a452400e8c43c2ac843effc00d5704f4c`. There is no configured Git remote. The server and schema were unchanged.

- Reproduced a mismatch where daily Ask facts counted two combined feed records while the log query counted one session. The regression test now checks session counts by the first feed's day and bottle volume by each step's actual day.
- Reproduced a live Foundation Models context-window error for “Hey” and rejected the label-shaped response shown in the user's screenshot. Synthetic live-model checks passed for a greeting, yesterday's care summary, and bottle volume in the last 24 hours. The latter uses a model-written answer from the exact local aggregate, with source-linked facts retained for validation.
- `make test` passed with the expected 978 Swift package tests. `make build` passed for iOS Simulator. A generic iOS device build from `b2a13f7` succeeded, and `codesign --verify --deep --strict` passed with keychain access. The app executable SHA-256 is `6e50aae081fdf8a6c582f4491f0c942428db5ca5ea33a45573964c158642d2dc`.
- `devicectl device install app` succeeded on `Phone A` and `Phone B`. Read-only installed-app queries on both returned `com.olhapi.babytracker` version 1.0, bundle version 1. Neither app was launched by this verification.

The live-model fixture checks ran on this Mac with synthetic data. The answers and interaction on either physical iPhone, and the user's actual private log question, have not been verified after installation.

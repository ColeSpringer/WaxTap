module github.com/colespringer/waxtap/v3

go 1.26.0

// Shipped a session-rotation API (ResetGuestIdentity) that was replaced by
// RotateIdentity the same day; use v3.0.1 or later.
retract v3.0.0

require (
	github.com/colespringer/waxflow v0.0.0-20261111201239-c9d965942277
	github.com/colespringer/waxlabel v1.8.0
	github.com/dop251/goja v0.0.0-20261103214539-f5d1d0025744
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/dlclark/regexp2/v2 v2.12.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20261103193413-97e8293de3c4 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

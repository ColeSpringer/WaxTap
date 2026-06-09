module github.com/colespringer/waxtap

go 1.26.3

retract (
	v1.4.3 // WEB SABR audio path non-functional (UMP varint decode bug); fixed in v1.5.0
	v1.4.2 // nsig solve fails on player_es6_tce (n=undefined); WEB SABR still broken; fixed in v1.5.0
	v1.4.1 // WEB SABR audio path non-functional; fixed in v1.5.0
	v1.4.0 // WEB SABR audio path non-functional (n=undefined / sts=0); fixed in v1.5.0
	v1.0.0 // download throttling bug; use v1.0.1 or later
)

require (
	github.com/dop251/goja v0.0.0-20260311135729-065cd970411c
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.9
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	golang.org/x/text v0.35.0 // indirect
)

package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	wferr "github.com/colespringer/waxflow/waxerr"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Two packages are named waxerr here: wferr is WaxFlow's error *codes*, waxerr is
// WaxTap's own sentinels. This file is the translation between them.

// classifyEngineError maps a WaxFlow failure onto the WaxTap sentinel that gives
// it the right exit code, and returns err unchanged for anything WaxTap has no
// mapping for.
//
// Without it only demuxer-open failures were classified, so an encoder-side
// refusal (a 6-channel ALAC spec) fell through to exit 1 / code "error" while
// every other unsupported-spec case in the CLI exits 2. The mapping is checked
// against WaxFlow's own ExitContract so the two taxonomies agree rather than
// merely resemble each other:
//
//	CodeUnsupportedFormat                    unsupported  ErrIncompatibleSpec   exit 2
//	CodeUnsupportedSource                    unsupported  ErrUnsupportedInput   exit 2
//	CodeInvalidRequest, CodePayloadTooLarge  invalid      ErrIncompatibleSpec   exit 2
//	CodeSourceUnreadable                     io           *fs.PathError         exit 10
//	CodeOutputUnwritable                     io           *tempfile.OutputError exit 10
//	CodeCanceled                             canceled     context.Canceled      exit 130
//	everything else                          -            unmapped              exit 1
//
// CodeUnsupportedFormat is overloaded upstream: it marks both an encoder refusing
// a spec and a container rejecting malformed bytes. It maps to the spec sentinel
// here, which is the case only this function can reach, and classifyInputError
// overrides it for the sites that can only be reading.
//
// The two I/O codes are I/O in WaxFlow's taxonomy, not "unsupported", and they
// stay that way here: a genuine read failure (a bad disk, a file truncated
// mid-read) is not an invalid request, and the exit code is the contract.
// CodeNotFound comes only from WaxFlow's HTTP client and CodeOverloaded from a
// server WaxTap does not run, so both are deliberately unmapped.
//
// input and output name the files for the I/O cases; an empty one leaves the
// error unmapped rather than reporting a path error with no path.
func classifyEngineError(err error, input, output string) error {
	if err == nil {
		return nil
	}
	switch wferr.CodeOf(err) {
	case wferr.CodeUnsupportedFormat, wferr.CodeInvalidRequest, wferr.CodePayloadTooLarge:
		return fmt.Errorf("%w: %v", waxerr.ErrIncompatibleSpec, err)
	case wferr.CodeUnsupportedSource:
		return fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	case wferr.CodeSourceUnreadable:
		if input == "" {
			return err
		}
		return &fs.PathError{Op: "read", Path: input, Err: err}
	case wferr.CodeOutputUnwritable:
		if output == "" {
			return err
		}
		return tempfile.WrapOutput("write", &fs.PathError{Op: "write", Path: output, Err: err})
	case wferr.CodeCanceled:
		// CodeOf folds a context error in the chain into this code, in which case the
		// error already reports the cancellation; an explicitly coded one does not.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %v", context.Canceled, err)
	}
	return err
}

// classifyInputError is classifyEngineError for operations that can only fail on
// the input: probing, demuxer open, and analysis. It defaults to
// ErrUnsupportedInput, which is what those call sites already carried, so an
// unclassified failure there stays a statement about the file rather than
// falling through to exit 1.
//
// The default also covers CodeUnsupportedFormat, which WaxFlow overloads: every
// container and codec's malformed() helper carries it, and so does an encoder
// refusing a spec it cannot serve. The code cannot separate a truncated file from
// a 6-channel ALAC request, but the call site can, and these sites only ever read.
//
// It names the codes it delegates rather than testing whether the delegate
// changed the error: classifyEngineError deliberately returns some errors
// untouched (an I/O code with no path to name, a cancellation that already reports
// itself), and an identity test would then wrap a cancellation as bad input and
// sever errors.Is with it.
func classifyInputError(err error, input string) error {
	if err == nil {
		return nil
	}
	switch wferr.CodeOf(err) {
	case wferr.CodeCanceled, wferr.CodeInvalidRequest, wferr.CodePayloadTooLarge:
		return classifyEngineError(err, input, "")
	case wferr.CodeSourceUnreadable:
		// With no path there is no path error to build, so it falls to the default:
		// still exit 2, which is where these sites have always put it.
		if input != "" {
			return classifyEngineError(err, input, "")
		}
	}
	return fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
}

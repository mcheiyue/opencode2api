package main

import (
	"context"
	"errors"
	"io"
)

func streamErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "other"
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func streamTermination(obs *streamEndObserver, copyErr error) string {
	if obs != nil && obs.readErr != nil {
		switch {
		case errors.Is(obs.readErr, io.ErrUnexpectedEOF):
			return "upstream_unexpected_eof"
		case errors.Is(obs.readErr, context.Canceled):
			return "read_context_canceled"
		case errors.Is(obs.readErr, context.DeadlineExceeded):
			return "upstream_deadline_exceeded"
		default:
			return "upstream_read_error"
		}
	}
	if copyErr != nil {
		if errors.Is(copyErr, context.Canceled) {
			return "downstream_context_canceled"
		}
		return "downstream_write_error"
	}
	if obs != nil && obs.sawTerminal {
		return "completed"
	}
	if obs != nil && obs.hasContent {
		return "clean_without_terminal"
	}
	return "empty_without_terminal"
}

func streamCompletion(obs *streamEndObserver) string {
	if obs == nil || obs.sawTerminal {
		return "completed"
	}
	if obs.hasContent {
		return "premature_close"
	}
	return "empty_response"
}

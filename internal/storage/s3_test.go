package storage

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestIsNotFound(t *testing.T) {
	missing := minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}
	denied := minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "bare minio not-found", err: missing, want: true},
		// Every read annotates its error with the object key. The SDK's own
		// ToErrorResponse type-asserts without unwrapping, so a wrapped
		// not-found used to read as a real failure and a missing index cell
		// became a hard error instead of an empty region.
		{name: "wrapped minio not-found", err: fmt.Errorf(`get object "x": %w`, missing), want: true},
		{name: "doubly wrapped", err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", missing)), want: true},
		{name: "sentinel", err: fmt.Errorf("no such key: %w", ErrNotFound), want: true},
		{name: "permission failure is not absence", err: fmt.Errorf("get: %w", denied), want: false},
		{name: "unrelated error", err: errors.New("connection reset"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNotFound(tc.err); got != tc.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAsNotFound_WrapsOnlyAbsence(t *testing.T) {
	missing := minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}

	wrapped := asNotFound(fmt.Errorf(`get object "x": %w`, missing))
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("an absence error should wrap ErrNotFound so any caller can test for it")
	}

	real := errors.New("connection reset")
	if errors.Is(asNotFound(real), ErrNotFound) {
		t.Error("a genuine failure must not be reported as absence")
	}
}

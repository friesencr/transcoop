package farm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIErrorCancellationIsNotLoggedAsFailure(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, err := range []error{context.Canceled, fmt.Errorf("filesystem read: %w", context.Canceled)} {
		recorder := httptest.NewRecorder()
		apiError(recorder, err)
		if recorder.Code != 499 {
			t.Fatalf("cancellation status = %d", recorder.Code)
		}
		if output.Len() != 0 {
			t.Fatalf("cancellation logged as failure: %s", output.String())
		}
	}
}
func TestAPIErrorStillReportsRealFailures(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, tt := range []struct {
		err    error
		status int
	}{{errors.New("source changed during read"), 400}, {ErrLease, 409}} {
		output.Reset()
		recorder := httptest.NewRecorder()
		apiError(recorder, tt.err)
		if recorder.Code != tt.status || !strings.Contains(output.String(), tt.err.Error()) || !strings.Contains(recorder.Body.String(), tt.err.Error()) {
			t.Fatalf("failure lost: status=%d log=%q body=%q", recorder.Code, output.String(), recorder.Body.String())
		}
	}
}

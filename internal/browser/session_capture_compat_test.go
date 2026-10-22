package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
)

// Exercise the real Agent->Executor Unix/JSON transport with the BASE
// Executor's strict capture input shape and its old successful response
// (without capture_quality). No installed services or Browser are touched.
func TestBrowserSessionCaptureLegacyExecutorCompatibility(t *testing.T) {
	imagePixels := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			imagePixels.Set(x, y, color.RGBA{R: uint8(x * 12), G: uint8(y * 18), B: 90, A: 255})
		}
	}
	var jpegBuffer bytes.Buffer
	if err := jpeg.Encode(&jpegBuffer, imagePixels, &jpeg.Options{Quality: 40}); err != nil {
		t.Fatal(err)
	}
	payload := jpegBuffer.Bytes()
	digest := sha256.Sum256(payload)
	for _, tc := range []struct {
		name        string
		quality     int
		fit         bool
		cropX       int
		cropY       int
		maxHeight   int
		wantQuality int
		wantError   string
	}{
		{name: "default_quality", quality: 0, wantQuality: 40},
		{name: "explicit_quality", quality: 31, wantQuality: 31},
		{name: "unsupported_opt_in", quality: 40, fit: true, wantError: "browser_capture_fit_unavailable"},
		{name: "unsupported_region", quality: 40, cropX: 800, cropY: 50, maxHeight: 200, wantError: "browser_capture_region_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testBrowserManager(t)
			m.token = "fixture-executor-token"
			socket := filepath.Join(t.TempDir(), "legacy-executor.sock")
			m.cfg.ExecutorSocket = socket
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				conn, e := ln.Accept()
				if e != nil {
					done <- e
					return
				}
				defer conn.Close()
				var request executor.Request
				if e = json.NewDecoder(conn).Decode(&request); e != nil {
					done <- e
					return
				}
				if request.Action != "browser_session_capture" || request.Token != m.token ||
					request.SessionID != "legacy_session" || request.Workspace != m.cfg.WorkspaceDir ||
					request.Root || request.Approval {
					done <- fmt.Errorf("wrong authenticated Browser request: %+v", request)
					return
				}
				legacy := struct {
					Quality  int `json:"quality"`
					MaxWidth int `json:"max_width"`
				}{}
				decoder := json.NewDecoder(strings.NewReader(request.Content))
				decoder.DisallowUnknownFields() // EXACT BASE behavior
				e = decoder.Decode(&legacy)
				response := executor.Response{
					OK:             true,
					Status:         "captured",
					SessionID:      "legacy_session",
					MIMEType:       "image/jpeg",
					OutputEncoding: "base64",
					Output:         base64.StdEncoding.EncodeToString(payload),
					BytesSeen:      int64(len(payload)),
					BytesReturned:  int64(len(payload)),
					FileVersion:    "sha256:" + hex.EncodeToString(digest[:]),
					// The BASE Executor NEVER supplied capture_quality.
				}
				if tc.wantError != "" {
					if e == nil {
						done <- fmt.Errorf("BASE decoder silently accepted unsupported opt-in %s", tc.wantError)
						return
					}
					response = executor.Response{ErrorCode: "invalid_browser_capture", ErrorClass: "validation",
						Error: "quality 15..70 and max_width 320..1024 required"}
				} else if e != nil || legacy.Quality != tc.wantQuality || legacy.MaxWidth != 640 {
					done <- fmt.Errorf("BASE decoder or parameters changed: %+v, err=%v", legacy, e)
					return
				}
				done <- json.NewEncoder(conn).Encode(response)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := m.SessionCapture(ctx, SessionOptions{
				Workspace: m.cfg.WorkspaceDir, SessionID: "legacy_session",
				CaptureQuality: tc.quality, CaptureFitQuality: tc.fit,
				CaptureCropX: tc.cropX, CaptureCropY: tc.cropY, CaptureMaxHeight: tc.maxHeight,
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-ctx.Done():
				t.Fatal("legacy Executor fixture did not finish")
			}
			if tc.wantError != "" {
				if got.OK || got.ErrorCode != tc.wantError || got.Output != "" || got.CaptureQuality != 0 ||
					got.Status != "not_captured" || got.SessionID != "legacy_session" {
					t.Fatalf("unsupported BASE Executor returned a fitted image: %+v", got)
				}
				return
			}
			if !got.OK || got.CaptureQuality != tc.wantQuality ||
				got.MIMEType != "image/jpeg" || got.BytesReturned != int64(len(payload)) ||
				got.FileVersion != "sha256:"+hex.EncodeToString(digest[:]) ||
				got.Output != base64.StdEncoding.EncodeToString(payload) {
				t.Fatalf("HEAD Agent -> BASE Executor lost legacy one-shot delivery or truthful quality: %+v", got)
			}
		})
	}
}

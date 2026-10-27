package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidatePublicationIDRejectsTruncatedModernShortcodes(t *testing.T) {
	nextCalls := 0
	handler := validatePublicationIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{
		"/reel/DYDnF3BPeL",
		"/reels/DYDnF3BPe",
		"/tv/DYDnF3B",
		"/creator/reel/DYDnF3BP",
		"/api/DYDnF3BPeL?kind=reel",
		"/api/DYDnF3BPeL?prefer=video",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", path, recorder.Code, http.StatusBadRequest)
		}
	}
	if nextCalls != 0 {
		t.Fatalf("next handler called %d times for invalid IDs", nextCalls)
	}
}

func TestValidatePublicationIDAcceptsModernAndHistoricalPostShortcodes(t *testing.T) {
	nextCalls := 0
	handler := validatePublicationIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	}))

	paths := []string{
		"/reel/DYDnF3BPeLr",
		"/reels/DbQM5BJsFGI",
		"/tv/BkvTIkEDkXs",
		"/p/G",
		"/creator/p/Ab_9-",
		"/api/G",
		"/stories/creator/3946405852517198393",
	}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Errorf("%s status = %d, want %d", path, recorder.Code, http.StatusNoContent)
		}
	}
	if nextCalls != len(paths) {
		t.Fatalf("next handler called %d times, want %d", nextCalls, len(paths))
	}
}

func TestValidatePublicationIDRejectsInvalidAlphabetAndOversizedIDs(t *testing.T) {
	handler := validatePublicationIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not receive an invalid ID")
	}))

	for _, path := range []string{
		"/p/not.valid",
		"/p/ABCDEFGHIJKL",
		"/stories/creator/not-a-number",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", path, recorder.Code, http.StatusBadRequest)
		}
	}
}

func TestClientIPCloudflareCrossZoneWorkerOnlyBehindTrustedProxy(t *testing.T) {
	local := httptest.NewRequest(http.MethodGet, "https://preview.example.test/offload/ABCDEF/1", nil)
	local.RemoteAddr = net.JoinHostPort("127.0.0.1", "12345")
	local.Header.Set("CF-Worker", "preview.example.test")
	local.Header.Set("CF-Connecting-IP", cloudflareCrossZoneWorkerIP)
	if got := clientIP(local); got != unknownProxyClient {
		t.Fatalf("local worker sentinel: got %q, want %q", got, unknownProxyClient)
	}

	external := httptest.NewRequest(http.MethodGet, "https://preview.example.test/reel/ABCDEF", nil)
	external.RemoteAddr = net.JoinHostPort("203.0.113.8", "12345")
	external.Header.Set("CF-Worker", "spoofed.example.test")
	external.Header.Set("CF-Connecting-IP", cloudflareCrossZoneWorkerIP)
	if got := clientIP(external); got != "203.0.113.8" {
		t.Fatalf("external spoof must not change rate-limit identity: got %q", got)
	}
}

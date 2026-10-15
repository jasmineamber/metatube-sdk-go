package fc2hub

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metatube-community/metatube-sdk-go/internal/envconfig"
	"github.com/metatube-community/metatube-sdk-go/provider/internal/scraper"
	"github.com/metatube-community/metatube-sdk-go/provider/internal/testkit"
	"golang.org/x/text/language"
)

func TestFC2HUB_NormalizeMovieURL(t *testing.T) {
	fc2hub := New()
	got, err := fc2hub.normalizeMovieURL("http://javten.com/video/1794095/id4304428/title")
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://javten.com/video/1794095/id4304428/title"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFC2HUB_GetMovieInfoByID(t *testing.T) {
	testkit.Test(t, New, []string{
		"1152468-2725031",
		"1258474-3104947",
		"1258463-3104926",
		"1258427-3104805",
		"1258427-3104805",
		"230929-803681",
		"1259441-3106475",
	})
}

func TestFC2HUB_SearchMovie(t *testing.T) {
	testkit.Test(t, New, []string{
		"FC2-PPV-2725031",
		"fc2-2417378",
	})
}

func TestFC2HUB_BootstrapCookies(t *testing.T) {
	const target = "https://javten.com/video/123/id456/title?x=1&y=2"
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"cookies":{"cf_clearance":"clearance-value"},"user_agent":"cf-agent"}`)
	}))
	defer server.Close()

	fc2hub := newTestFC2HUB(nil)
	fc2hub.cookieURL = server.URL + "/cookies"
	session := fc2hub.bootstrapCookies(target)
	if session == nil {
		t.Fatal("expected cookie session")
	}
	if got := rawQuery; got != "url=https%3A%2F%2Fjavten.com%2Fvideo%2F123%2Fid456%2Ftitle%3Fx%3D1%26y%3D2" {
		t.Fatalf("unexpected query: %q", got)
	}

	c := fc2hub.ClonedCollector()
	fc2hub.applyCookieSession(c, session)
	if got := c.UserAgent; got != "cf-agent" {
		t.Fatalf("user agent = %q, want cf-agent", got)
	}
	for _, target := range []string{
		"https://javten.com/search?kw=test",
		"https://javten.com/video/123/id456/title",
	} {
		cookies := c.Cookies(target)
		if len(cookies) != 1 || cookies[0].Name != "cf_clearance" || cookies[0].Value != "clearance-value" {
			t.Fatalf("cookies for %s = %#v", target, cookies)
		}
	}
}

func TestFC2HUB_BootstrapCookiesFailureAndCache(t *testing.T) {
	var (
		mu       sync.Mutex
		status   = http.StatusOK
		response = `{"cookies":{"cf_clearance":"first"},"user_agent":"first-agent"}`
		now      = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()

	fc2hub := newTestFC2HUB(nil)
	fc2hub.cookieURL = server.URL
	fc2hub.now = func() time.Time { return now }
	session := fc2hub.bootstrapCookies("https://javten.com/search?kw=x")
	if session == nil {
		t.Fatal("expected initial cookie session")
	}
	fc2hub.applyCookieSession(fc2hub.ClonedCollector(), session)

	for _, test := range []struct {
		name     string
		status   int
		response string
	}{
		{name: "non-200", status: http.StatusInternalServerError, response: `{}`},
		{name: "malformed-json", status: http.StatusOK, response: `{`},
		{name: "empty-cookies", status: http.StatusOK, response: `{"cookies":{},"user_agent":"agent"}`},
		{name: "missing-user-agent", status: http.StatusOK, response: `{"cookies":{"x":"y"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			status, response = test.status, test.response
			mu.Unlock()
			if got := fc2hub.bootstrapCookies("https://javten.com/search?kw=x"); got != session {
				t.Fatal("expected unexpired cached session fallback")
			}
		})
	}

	now = now.Add(cookieSessionTTL)
	mu.Lock()
	status, response = http.StatusInternalServerError, `{}`
	mu.Unlock()
	if got := fc2hub.bootstrapCookies("https://javten.com/search?kw=x"); got != nil {
		t.Fatal("expected expired session not to be reused")
	}
	directCollector := fc2hub.ClonedCollector()
	fc2hub.applyCookieSession(directCollector, nil)
	if got := directCollector.Cookies(cookieOrigin); len(got) != 0 {
		t.Fatalf("expired integration cookies remain in jar: %#v", got)
	}
}

func TestFC2HUB_SetConfigAndRequestTimeout(t *testing.T) {
	fc2hub := newTestFC2HUB(nil)
	config := envconfig.NewConfig()
	config.Set("cf_cookie_url", "https://cookies.example.test/cookies")
	if err := fc2hub.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	if got := fc2hub.cookieURL; got != "https://cookies.example.test/cookies" {
		t.Fatalf("cookie URL = %q", got)
	}

	for _, invalid := range []string{"", "/cookies", "ftp://cookies.example.test/cookies"} {
		config.Set("cf_cookie_url", invalid)
		if err := fc2hub.SetConfig(config); err == nil {
			t.Errorf("SetConfig(%q) unexpectedly succeeded", invalid)
		}
	}

	const timeout = 17 * time.Second
	fc2hub.SetRequestTimeout(timeout)
	if got := fc2hub.cookieClient.Timeout; got != timeout {
		t.Fatalf("cookie client timeout = %s, want %s", got, timeout)
	}
}

func TestFC2HUB_SearchMovieReusesSessionForRedirect(t *testing.T) {
	var (
		cookieRequests atomic.Int32
		targetRequests atomic.Int32
	)
	cookieServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieRequests.Add(1)
		if got := r.URL.Query().Get("url"); got != "https://javten.com/search?kw=FC2-123456" {
			t.Errorf("bootstrap target = %q", got)
		}
		_, _ = io.WriteString(w, `{"cookies":{"cf_clearance":"session-cookie"},"user_agent":"session-agent"}`)
	}))
	defer cookieServer.Close()

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		targetRequests.Add(1)
		if got := req.Header.Get("User-Agent"); got != "session-agent" {
			t.Errorf("target user agent = %q, want session-agent", got)
		}
		if got := req.Header.Get("Cookie"); !strings.Contains(got, "cf_clearance=session-cookie") {
			t.Errorf("target Cookie header = %q", got)
		}
		status := http.StatusOK
		header := make(http.Header)
		body := `<html><head><script type="application/ld+json">{"@type":"Movie","name":"Test title","identifier":["FC2-123456"]}</script></head><body></body></html>`
		if req.URL.Path == "/search" {
			status = http.StatusFound
			header.Set("Location", "/video/123/id456/title")
			body = ""
		}
		return responseFor(req, status, header, body), nil
	})
	fc2hub := newTestFC2HUB(transport)
	fc2hub.cookieURL = cookieServer.URL + "/cookies"

	results, err := fc2hub.SearchMovie("FC2-123456")
	if err != nil {
		t.Fatal(err)
	}
	if got := cookieRequests.Load(); got != 1 {
		t.Fatalf("cookie requests = %d, want 1", got)
	}
	if got := targetRequests.Load(); got != 2 {
		t.Fatalf("target requests = %d, want 2", got)
	}
	if len(results) != 1 {
		t.Fatalf("unexpected results: %#v", results)
	}
}

func TestFC2HUB_ScrapeSerializesCookieSessions(t *testing.T) {
	var (
		cookieSequence atomic.Int32
		targetMismatch atomic.Int32
	)
	cookieServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sequence := cookieSequence.Add(1)
		_, _ = fmt.Fprintf(w, `{"cookies":{"session":"%d"},"user_agent":"agent-%d"}`, sequence, sequence)
	}))
	defer cookieServer.Close()

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var cookieValue string
		for _, cookie := range req.Cookies() {
			if cookie.Name == "session" {
				cookieValue = cookie.Value
			}
		}
		if req.Header.Get("User-Agent") != "agent-"+cookieValue {
			targetMismatch.Add(1)
		}
		return responseFor(req, http.StatusOK, make(http.Header), `<html><body></body></html>`), nil
	})
	fc2hub := newTestFC2HUB(transport)
	fc2hub.cookieURL = cookieServer.URL

	const callers = 8
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fc2hub.SearchMovie("test"); err != nil {
				t.Errorf("SearchMovie: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := cookieSequence.Load(); got != callers {
		t.Fatalf("cookie requests = %d, want %d", got, callers)
	}
	if got := targetMismatch.Load(); got != 0 {
		t.Fatalf("cookie/user-agent mismatches = %d", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func responseFor(req *http.Request, status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func newTestFC2HUB(transport http.RoundTripper) *FC2HUB {
	opts := []scraper.Option{}
	if transport != nil {
		opts = append(opts, scraper.WithTransport(transport))
	}
	return &FC2HUB{
		Scraper:            scraper.NewDefaultScraper(Name, baseURL, Priority, language.Japanese, opts...),
		cookieURL:          defaultCookieURL,
		cookieClient:       &http.Client{Timeout: defaultTimeout},
		managedCookieNames: make(map[string]struct{}),
		now:                time.Now,
	}
}

func TestFC2HUB_CookieTargetQueryEncoding(t *testing.T) {
	target := "https://javten.com/search?kw=FC2 123&sort=latest"
	parsed, err := url.Parse(defaultCookieURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("url", target)
	parsed.RawQuery = query.Encode()
	if !strings.Contains(parsed.RawQuery, "url=https%3A%2F%2Fjavten.com%2Fsearch%3Fkw%3DFC2+123%26sort%3Dlatest") {
		t.Fatalf("unexpected encoded URL query: %q", parsed.RawQuery)
	}
}

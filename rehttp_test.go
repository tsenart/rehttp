package rehttp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TODO: test with context.Context that cancels the request,
// test with transport-level timeouts.

type mockRoundTripper struct {
	t *testing.T

	mu     sync.Mutex
	calls  int
	ccalls int
	bodies []string
	retFn  func(int, *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()

	att := m.calls
	m.calls++
	if req.Body != nil {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, req.Body)
		req.Body.Close()
		require.Nil(m.t, err)
		m.bodies = append(m.bodies, buf.String())
	}
	m.mu.Unlock()

	return m.retFn(att, req)
}

func (m *mockRoundTripper) CancelRequest(req *http.Request) {
	m.mu.Lock()
	m.ccalls++
	m.mu.Unlock()
}

func (m *mockRoundTripper) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockRoundTripper) CancelCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ccalls
}

func (m *mockRoundTripper) Bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, r.URL.Path)
	}))
	defer srv.Close()

	c := &http.Client{
		Transport: NewTransport(nil, RetryTemporaryErr(2), ConstDelay(time.Second)),
		Timeout:   time.Second,
	}
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, res)
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		nerr, ok := uerr.Err.(net.Error)
		require.True(t, ok)
		assert.True(t, nerr.Timeout())
	}
}

func TestClientRetry(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	client := &http.Client{
		Transport: NewTransport(mock, RetryTemporaryErr(1), NoDelay()),
	}
	_, err := client.Get("http://example.com")
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
	assert.Equal(t, 0, mock.CancelCalls())
}

func TestClientFailBufferBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	client := &http.Client{
		Transport: NewTransport(mock, RetryTemporaryErr(1), NoDelay()),
	}
	_, err := client.Post("http://example.com", "text/plain", iotest.TimeoutReader(strings.NewReader("hello")))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, iotest.ErrTimeout, uerr.Err)
	}
	assert.Equal(t, 0, mock.Calls())
	assert.Equal(t, 0, mock.CancelCalls())
}

func TestClientPreventRetryWithBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	tr := NewTransport(mock, RetryTemporaryErr(1), NoDelay())
	tr.PreventRetryWithBody = true
	client := &http.Client{
		Transport: tr,
	}

	_, err := client.Post("http://example.com", "text/plain", strings.NewReader("test"))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 1, mock.Calls()) // did not retry
	assert.Equal(t, 0, mock.CancelCalls())
	assert.Equal(t, []string{"test"}, mock.Bodies())
}

func TestClientRetryWithBody(t *testing.T) {
	retFn := func(att int, req *http.Request) (*http.Response, error) {
		return nil, tempErr{}
	}
	mock := &mockRoundTripper{t: t, retFn: retFn}

	client := &http.Client{
		Transport: NewTransport(mock, RetryTemporaryErr(1), NoDelay()),
	}
	_, err := client.Post("http://example.com", "text/plain", strings.NewReader("hello"))
	if assert.NotNil(t, err) {
		uerr, ok := err.(*url.Error)
		require.True(t, ok)
		assert.Equal(t, tempErr{}, uerr.Err)
	}
	assert.Equal(t, 2, mock.Calls())
	assert.Equal(t, 0, mock.CancelCalls())
	assert.Equal(t, []string{"hello", "hello"}, mock.Bodies())
}

func TestClientNoRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	}))
	defer srv.Close()

	c := &http.Client{
		Transport: NewTransport(nil, RetryTemporaryErr(2), ConstDelay(time.Second)),
	}
	res, err := c.Get(srv.URL + "/test")
	require.Nil(t, err)
	defer res.Body.Close()

	assert.Equal(t, 200, res.StatusCode)
	var buf bytes.Buffer
	_, err = io.Copy(&buf, res.Body)
	require.Nil(t, err)
	assert.Equal(t, "/test", buf.String())
}

func TestNoDelay(t *testing.T) {
	fn := NoDelay()
	want := time.Duration(0)
	for i := 0; i < 5; i++ {
		delay := fn(nil, nil, i, nil)
		assert.Equal(t, want, delay, "%d", i)
	}
}

func TestConstDelay(t *testing.T) {
	want := 2 * time.Second
	fn := ConstDelay(want)
	for i := 0; i < 5; i++ {
		delay := fn(nil, nil, i, nil)
		assert.Equal(t, want, delay, "%d", i)
	}
}

func TestLinearDelay(t *testing.T) {
	initial := 2 * time.Second
	fn := LinearDelay(initial)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second, 10 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}
}

func TestExponentialDelay(t *testing.T) {
	initial := 2 * time.Second
	fn := ExponentialDelay(initial, time.Second)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}

	initial = 100 * time.Millisecond
	fn = ExponentialDelay(initial, 10*time.Millisecond)
	want = []time.Duration{100 * time.Millisecond, time.Second, 10 * time.Second}
	for i := 0; i < len(want); i++ {
		got := fn(nil, nil, i, nil)
		assert.Equal(t, want[i], got, "%d", i)
	}
}

func TestRetryHTTPMethods(t *testing.T) {
	cases := []struct {
		retries int
		meths   []string
		inMeth  string
		att     int
		want    bool
	}{
		{retries: 1, meths: nil, inMeth: "GET", att: 0, want: false},
		{retries: 0, meths: nil, inMeth: "GET", att: 1, want: false},
		{retries: 1, meths: []string{"get"}, inMeth: "GET", att: 0, want: true},
		{retries: 1, meths: []string{"GET"}, inMeth: "GET", att: 0, want: true},
		{retries: 1, meths: []string{"GET"}, inMeth: "POST", att: 0, want: false},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 0, want: true},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 1, want: true},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "POST", att: 2, want: false},
		{retries: 2, meths: []string{"GET", "POST"}, inMeth: "put", att: 0, want: false},
		{retries: 2, meths: []string{"GET", "POST", "PUT"}, inMeth: "put", att: 0, want: true},
	}

	for i, tc := range cases {
		fn := RetryHTTPMethods(tc.retries, tc.meths...)
		req, err := http.NewRequest(tc.inMeth, "", nil)
		require.Nil(t, err)
		got := fn(req, nil, tc.att, nil)
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

func TestRetryStatus500(t *testing.T) {
	cases := []struct {
		retries int
		res     *http.Response
		att     int
		want    bool
	}{
		{retries: 1, res: nil, att: 0, want: false},
		{retries: 1, res: nil, att: 1, want: false},
		{retries: 1, res: &http.Response{StatusCode: 200}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 400}, att: 0, want: false},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 1, res: &http.Response{StatusCode: 500}, att: 1, want: false},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 0, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 1, want: true},
		{retries: 2, res: &http.Response{StatusCode: 500}, att: 2, want: false},
	}

	for i, tc := range cases {
		fn := RetryStatus500(tc.retries)
		got := fn(nil, tc.res, tc.att, nil)
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

type tempErr struct{}

func (t tempErr) Error() string   { return "temp error" }
func (t tempErr) Temporary() bool { return true }

func TestRetryTemporaryErr(t *testing.T) {
	cases := []struct {
		retries int
		err     error
		att     int
		want    bool
	}{
		{retries: 1, err: nil, att: 0, want: false},
		{retries: 1, err: nil, att: 1, want: false},
		{retries: 1, err: io.EOF, att: 0, want: false},
		{retries: 1, err: tempErr{}, att: 0, want: true},
		{retries: 1, err: tempErr{}, att: 1, want: false},
	}

	for i, tc := range cases {
		fn := RetryTemporaryErr(tc.retries)
		got := fn(nil, nil, tc.att, tc.err)
		assert.Equal(t, tc.want, got, "%d", i)
	}
}

func TestRetryAll(t *testing.T) {
	status := RetryStatus500(2)
	temp := RetryTemporaryErr(2)
	meths := RetryHTTPMethods(2, "GET")
	fn := RetryAll(status, temp, meths)

	cases := []struct {
		method string
		status int
		att    int
		err    error
		want   bool
	}{
		{"POST", 200, 0, nil, false},
		{"GET", 200, 0, nil, false},
		{"GET", 500, 0, nil, false},
		{"GET", 500, 0, tempErr{}, true},
		{"GET", 500, 1, tempErr{}, true},
		{"GET", 500, 2, tempErr{}, false},
		{"GET", 400, 0, tempErr{}, false},
		{"GET", 500, 0, io.EOF, false},
	}
	for i, tc := range cases {
		got := fn(&http.Request{Method: tc.method}, &http.Response{StatusCode: tc.status}, tc.att, tc.err)
		assert.Equal(t, tc.want, got, "%d", i)
	}

	// en empty RetryAll always returns true
	fn = RetryAll()
	got := fn(nil, nil, 0, nil)
	assert.True(t, got, "empty RetryAll")
}

func TestRetryAny(t *testing.T) {
	status := RetryStatus500(2)
	temp := RetryTemporaryErr(2)
	meths := RetryHTTPMethods(2, "GET")
	fn := RetryAny(status, temp, meths)

	cases := []struct {
		method string
		status int
		att    int
		err    error
		want   bool
	}{
		{"POST", 200, 0, nil, false},
		{"GET", 200, 0, nil, true},
		{"POST", 500, 0, nil, true},
		{"POST", 200, 0, tempErr{}, true},
		{"POST", 200, 0, io.EOF, false},
		{"GET", 500, 0, tempErr{}, true},
		{"GET", 500, 1, tempErr{}, true},
		{"GET", 500, 2, tempErr{}, false},
	}
	for i, tc := range cases {
		got := fn(&http.Request{Method: tc.method}, &http.Response{StatusCode: tc.status}, tc.att, tc.err)
		assert.Equal(t, tc.want, got, "%d", i)
	}

	// en empty RetryAny always returns false
	fn = RetryAny()
	got := fn(nil, nil, 0, nil)
	assert.False(t, got, "empty RetryAny")
}

func TestToRetryFn(t *testing.T) {
	fn := ToRetryFn(RetryTemporaryErr(2), LinearDelay(time.Second))

	cases := []struct {
		err       error
		att       int
		wantRetry bool
		wantDelay time.Duration
	}{
		{err: nil, att: 0, wantRetry: false, wantDelay: 0},
		{err: io.EOF, att: 0, wantRetry: false, wantDelay: 0},
		{err: tempErr{}, att: 0, wantRetry: true, wantDelay: time.Second},
		{err: tempErr{}, att: 1, wantRetry: true, wantDelay: 2 * time.Second},
		{err: tempErr{}, att: 2, wantRetry: false, wantDelay: 0},
	}

	for i, tc := range cases {
		retry, delay := fn(nil, nil, tc.att, tc.err)
		assert.Equal(t, tc.wantRetry, retry, "%d - retry?", i)
		assert.Equal(t, tc.wantDelay, delay, "%d - delay", i)
	}
}

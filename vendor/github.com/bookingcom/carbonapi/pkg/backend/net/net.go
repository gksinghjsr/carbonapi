// Package net implements a backend that communicates over a network.
// It uses HTTP and protocol buffers for communication.
package net

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bookingcom/carbonapi/pkg/types"
	"github.com/bookingcom/carbonapi/pkg/types/encoding/carbonapi_v2"
	"github.com/bookingcom/carbonapi/util"

	"github.com/dgryski/go-expirecache"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type ErrHTTPCode int

func (e ErrHTTPCode) Error() string {
	switch e / 100 {
	case 4:
		return fmt.Sprintf("HTTP client error %d", e)

	case 5:
		return fmt.Sprintf("HTTP server error %d", e)

	default:
		return fmt.Sprintf("HTTP unknown error %d", e)
	}
}

// Backend represents a host that accepts requests for metrics over HTTP.
type Backend struct {
	address       string
	scheme        string
	client        *http.Client
	timeout       time.Duration
	limiter       chan struct{}
	logger        *zap.Logger
	paths         *expirecache.Cache
	pathExpirySec int32
}

// Config configures an HTTP backend.
//
// The only required field is Address, which must be of the form
// "address[:port]", where address is an IP address or a hostname.
// Address must be a point that can accept HTTP requests.
type Config struct {
	Address string // The backend address.

	// Optional fields
	Client             *http.Client  // The client to use to communicate with backend. Defaults to http.DefaultClient.
	Timeout            time.Duration // Set request timeout. Defaults to no timeout.
	Limit              int           // Set limit of concurrent requests to backend. Defaults to no limit.
	PathCacheExpirySec uint32        // Set time in seconds before items in path cache expire. Defaults to 10 minutes.
	Logger             *zap.Logger   // Logger to use. Defaults to a no-op logger.
}

var fmtProto = []string{"protobuf"}

// New creates a new backend from the given configuration.
func New(cfg Config) (*Backend, error) {
	b := &Backend{
		paths: expirecache.New(0),
	}

	if cfg.PathCacheExpirySec > 0 {
		b.pathExpirySec = int32(cfg.PathCacheExpirySec)
	} else {
		b.pathExpirySec = int32(10 * time.Minute / time.Second)
	}

	address, scheme, err := parseAddress(cfg.Address)
	if err != nil {
		return nil, err
	}

	b.address = address
	b.scheme = scheme

	if cfg.Timeout > 0 {
		b.timeout = cfg.Timeout
	} else {
		b.timeout = 0
	}

	if cfg.Client != nil {
		b.client = cfg.Client
	} else {
		b.client = http.DefaultClient
	}

	if cfg.Limit > 0 {
		b.limiter = make(chan struct{}, cfg.Limit)
	}

	if cfg.Logger != nil {
		b.logger = cfg.Logger
	} else {
		b.logger = zap.New(nil)
	}

	return b, nil
}

func parseAddress(address string) (string, string, error) {
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}

	u, err := url.Parse(address)
	if err != nil {
		return "", "", err
	}

	return u.Host, u.Scheme, nil
}

func (b Backend) url(path string) *url.URL {
	return &url.URL{
		Scheme: b.scheme,
		Host:   b.address,
		Path:   path,
	}
}

func (b Backend) Logger() *zap.Logger {
	return b.logger
}

func (b Backend) enter(ctx context.Context) error {
	if b.limiter == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()

	case b.limiter <- struct{}{}:
		// fallthrough
	}

	return nil
}

func (b Backend) leave() error {
	if b.limiter == nil {
		return nil
	}

	select {
	case <-b.limiter:
		// fallthrough
	default:
		// this should never happen, but let's not block forever if it does
		return errors.New("Unable to return value to limiter")
	}

	return nil
}

func (b Backend) setTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.timeout > 0 {
		return context.WithTimeout(ctx, b.timeout)
	}

	return context.WithCancel(ctx)
}

func (b Backend) request(ctx context.Context, u *url.URL, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest("GET", "", body)
	if err != nil {
		return nil, err
	}
	req.URL = u

	req = req.WithContext(ctx)
	req = util.MarshalCtx(ctx, req)

	return req, nil
}

func (b Backend) do(ctx context.Context, trace types.Trace, req *http.Request) (string, []byte, error) {
	t0 := time.Now()
	resp, err := b.client.Do(req)
	trace.AddHTTPCall(t0)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return "", nil, err
	}

	t1 := time.Now()
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	trace.AddReadBody(t1)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", body, ErrHTTPCode(resp.StatusCode)
	}

	return resp.Header.Get("Content-Type"), body, nil
}

// Call makes a call to a backend.
// If the backend timeout is positive, Call will override the context timeout
// with the backend timeout.
// Call ensures that the outgoing request has a UUID set.
func (b Backend) call(ctx context.Context, trace types.Trace, u *url.URL, body io.Reader) (string, []byte, error) {
	ctx, cancel := b.setTimeout(ctx)
	defer cancel()

	t0 := time.Now()
	err := b.enter(ctx)
	trace.AddLimiter(t0)
	if err != nil {
		return "", nil, err
	}

	defer func() {
		if err := b.leave(); err != nil {
			b.logger.Error("Backend limiter full",
				zap.String("host", b.address),
				zap.String("uuid", util.GetUUID(ctx)),
				zap.Error(err),
			)
		}
	}()

	t1 := time.Now()
	req, err := b.request(ctx, u, body)
	trace.AddMarshal(t1)
	if err != nil {
		return "", nil, err
	}

	return b.do(ctx, trace, req)
}

// Probe performs a single update of the backend's top-level domains.
func (b *Backend) Probe() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	request := types.NewFindRequest("*")
	matches, err := b.Find(ctx, request)
	if err != nil {
		return
	}

	for _, m := range matches.Matches {
		b.paths.Set(m.Path, struct{}{}, 0, b.pathExpirySec)
	}
}

// TODO(gmagnusson): Should Contains become something different, where instead
// of answering yes/no to whether the backend contains any of the given
// targets, it returns a filtered list of targets that the backend contains?
// Is it worth it to make the distinction? If go-carbon isn't too unhappy about
// looking up metrics that it doesn't have, we maybe don't need to do this.

// Contains reports whether the backend contains any of the given targets.
func (b Backend) Contains(targets []string) bool {
	for _, target := range targets {
		if _, ok := b.paths.Get(target); ok {
			return true
		}
	}

	return false
}

// Render fetches raw metrics from a backend.
func (b Backend) Render(ctx context.Context, request types.RenderRequest) ([]types.Metric, error) {
	from := request.From
	until := request.Until
	targets := request.Targets

	t0 := time.Now()
	u := b.url("/render")
	u, body := carbonapiV2RenderEncoder(u, from, until, targets)
	request.Trace.AddMarshal(t0)

	contentType, resp, err := b.call(ctx, request.Trace, u, body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Request timed out")
		}

		return nil, errors.Wrap(err, "HTTP call failed")
	}

	t1 := time.Now()
	defer func() {
		request.Trace.AddUnmarshal(t1)
	}()
	var metrics []types.Metric

	switch contentType {
	case "application/x-protobuf":
		metrics, err = carbonapi_v2.RenderDecoder(resp)

	case "application/json":
		// TODO(gmagnusson)

	case "application/pickle":
		// TODO(gmagnusson)

	case "application/x-msgpack":
		// TODO(gmagnusson)

	case "application/x-carbonapi-v3-pb":
		// TODO(gmagnusson)

	default:
		return nil, errors.Errorf("Unknown content type '%s'", contentType)
	}

	if err != nil {
		return metrics, errors.Wrap(err, "Unmarshal failed")
	}

	if len(metrics) == 0 {
		return nil, types.ErrMetricsNotFound
	}

	for _, metric := range metrics {
		b.paths.Set(metric.Name, struct{}{}, 0, b.pathExpirySec)
	}

	return metrics, nil
}

func carbonapiV2RenderEncoder(u *url.URL, from int32, until int32, targets []string) (*url.URL, io.Reader) {
	vals := url.Values{
		"target": targets,
		"format": fmtProto,
		"from":   []string{strconv.Itoa(int(from))},
		"until":  []string{strconv.Itoa(int(until))},
	}
	u.RawQuery = vals.Encode()

	return u, nil
}

// Info fetches metadata about a metric from a backend.
func (b Backend) Info(ctx context.Context, request types.InfoRequest) ([]types.Info, error) {
	metric := request.Target

	t0 := time.Now()
	u := b.url("/info")
	u, body := carbonapiV2InfoEncoder(u, metric)
	request.Trace.AddMarshal(t0)

	_, resp, err := b.call(ctx, request.Trace, u, body)
	if err != nil {
		return nil, errors.Wrap(err, "HTTP call failed")
	}

	single, err := carbonapi_v2.IsInfoResponse(resp)
	if err != nil {
		return nil, errors.Wrap(err, "Protobuf unmarshal failed")
	}

	t1 := time.Now()
	defer func() {
		request.Trace.AddUnmarshal(t1)
	}()
	var infos []types.Info
	if single {
		infos, err = carbonapi_v2.SingleInfoDecoder(resp, b.address)
	} else {
		infos, err = carbonapi_v2.MultiInfoDecoder(resp)
	}

	if err != nil {
		return nil, errors.Wrap(err, "Protobuf unmarshal failed")
	}

	if len(infos) == 0 {
		return nil, types.ErrInfoNotFound
	}

	return infos, nil
}

func carbonapiV2InfoEncoder(u *url.URL, metric string) (*url.URL, io.Reader) {
	vals := url.Values{
		"target": []string{metric},
		"format": fmtProto,
	}
	u.RawQuery = vals.Encode()

	return u, nil
}

// Find resolves globs and finds metrics in a backend.
func (b Backend) Find(ctx context.Context, request types.FindRequest) (types.Matches, error) {
	query := request.Query

	t0 := time.Now()
	u := b.url("/metrics/find")
	u, body := carbonapiV2FindEncoder(u, query)
	request.Trace.AddMarshal(t0)

	contentType, resp, err := b.call(ctx, request.Trace, u, body)
	if err != nil {
		return types.Matches{}, errors.Wrap(err, "HTTP call failed")
	}

	t1 := time.Now()
	defer func() {
		request.Trace.AddUnmarshal(t1)
	}()
	var matches types.Matches

	switch contentType {
	case "application/x-protobuf":
		matches, err = carbonapi_v2.FindDecoder(resp)

	case "application/json":
		// TODO(gmagnusson)

	case "application/pickle":
		// TODO(gmagnusson)

	case "application/x-msgpack":
		// TODO(gmagnusson)

	case "application/x-carbonapi-v3-pb":
		// TODO(gmagnusson)

	default:
		return types.Matches{}, errors.Errorf("Unknown content type '%s'", contentType)
	}

	if err != nil {
		return matches, errors.Wrap(err, "Protobuf unmarshal failed")
	}

	if len(matches.Matches) == 0 {
		return matches, types.ErrMatchesNotFound
	}

	for _, match := range matches.Matches {
		if match.IsLeaf {
			b.paths.Set(match.Path, struct{}{}, 0, b.pathExpirySec)
		}
	}

	return matches, nil
}

func carbonapiV2FindEncoder(u *url.URL, query string) (*url.URL, io.Reader) {
	vals := url.Values{
		"query":  []string{query},
		"format": fmtProto,
	}
	u.RawQuery = vals.Encode()

	return u, nil
}

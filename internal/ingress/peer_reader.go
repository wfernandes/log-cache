package ingress

import (
	"fmt"
	"time"

	"code.cloudfoundry.org/go-log-cache/rpc/logcache"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache/internal/store"
	"golang.org/x/net/context"
)

// PeerReader reads envelopes from peers. It implements
// logcache.IngressServer.
type PeerReader struct {
	put Putter
	get Getter
}

// Putter writes envelopes to the store.
type Putter func(*loggregator_v2.Envelope)

// Getter fetches envelopes based on the given criteria.
type Getter func(
	sourceID string,
	start time.Time,
	end time.Time,
	envelopeType store.EnvelopeType,
	limit int,
) []*loggregator_v2.Envelope

// NewPeerReader creates and returns a new PeerReader.
func NewPeerReader(p Putter, g Getter) *PeerReader {
	return &PeerReader{
		put: p,
		get: g,
	}
}

// Send takes in data from the peer and submits it to the store.
func (r *PeerReader) Send(ctx context.Context, req *logcache.SendRequest) (*logcache.SendResponse, error) {
	for _, e := range req.Envelopes.Batch {
		r.put(e)
	}
	return &logcache.SendResponse{}, nil
}

// Read returns data from the store.
func (r *PeerReader) Read(ctx context.Context, req *logcache.ReadRequest) (*logcache.ReadResponse, error) {
	if req.EndTime != 0 && req.StartTime > req.EndTime {
		return nil, fmt.Errorf("StartTime (%d) must be before EndTime (%d)", req.StartTime, req.EndTime)
	}

	if req.Limit > 1000 {
		return nil, fmt.Errorf("Limit (%d) must be 1000 or less", req.Limit)
	}

	if req.EndTime == 0 {
		req.EndTime = time.Now().UnixNano()
	}

	if req.Limit == 0 {
		req.Limit = 100
	}

	envs := r.get(
		req.SourceId,
		time.Unix(0, req.StartTime),
		time.Unix(0, req.EndTime),
		r.convertEnvelopeType(req.EnvelopeType),
		int(req.Limit),
	)
	resp := &logcache.ReadResponse{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: envs,
		},
	}

	return resp, nil
}

func (r *PeerReader) convertEnvelopeType(t logcache.EnvelopeTypes) store.EnvelopeType {
	switch t {
	case logcache.EnvelopeTypes_LOG:
		return &loggregator_v2.Log{}
	case logcache.EnvelopeTypes_COUNTER:
		return &loggregator_v2.Counter{}
	case logcache.EnvelopeTypes_GAUGE:
		return &loggregator_v2.Gauge{}
	case logcache.EnvelopeTypes_TIMER:
		return &loggregator_v2.Timer{}
	case logcache.EnvelopeTypes_EVENT:
		return &loggregator_v2.Event{}
	default:
		return nil
	}
}

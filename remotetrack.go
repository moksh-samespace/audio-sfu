package sfu

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/rtp"
)

type remoteTrack struct {
	context               context.Context
	cancel                context.CancelFunc
	mu                    sync.RWMutex
	track                 IRemoteTrack
	onRead                func(interceptor.Attributes, *rtp.Packet)
	onPLI                 func()
	bitrate               *atomic.Uint32
	previousBytesReceived *atomic.Uint64
	currentBytesReceived  *atomic.Uint64
	latestUpdatedTS       *atomic.Uint64
	lastPLIRequestTime    time.Time
	onEndedCallbacks      []func()
	statsGetter           stats.Getter
	onStatsUpdated        func(*stats.Stats)
	log                   logging.LeveledLogger
	rtppool               *rtppool.RTPPool
}

func newRemoteTrack(ctx context.Context, log logging.LeveledLogger, useBuffer bool, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), statsGetter stats.Getter, onStatsUpdated func(*stats.Stats), onRead func(interceptor.Attributes, *rtp.Packet), pool *rtppool.RTPPool, onNetworkConditionChanged func(networkmonitor.NetworkConditionType)) *remoteTrack {
	localctx, cancel := context.WithCancel(ctx)

	rt := &remoteTrack{
		context:               localctx,
		cancel:                cancel,
		mu:                    sync.RWMutex{},
		track:                 track,
		bitrate:               &atomic.Uint32{},
		previousBytesReceived: &atomic.Uint64{},
		currentBytesReceived:  &atomic.Uint64{},
		latestUpdatedTS:       &atomic.Uint64{},
		onEndedCallbacks:      make([]func(), 0),
		statsGetter:           statsGetter,
		onStatsUpdated:        onStatsUpdated,
		onPLI:                 onPLI,
		onRead:                onRead,
		log:                   log,
		rtppool:               pool,
	}

	if pliInterval > 0 {
		rt.enableIntervalPLI(pliInterval)
	}

	go rt.readRTP()

	return rt
}

func (t *remoteTrack) Context() context.Context {
	return t.context
}

func (t *remoteTrack) readRTP() {
	readCtx, cancel := context.WithCancel(t.context)

	defer cancel()

	defer t.cancel()

	defer t.onEnded()

	for {
		select {
		case <-readCtx.Done():
			return
		default:
			if err := t.track.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
				t.log.Errorf("remotetrack: set read deadline error - %s", err.Error())
				return
			}
			buffer := t.rtppool.GetPayload()

			n, attrs, readErr := t.track.Read(*buffer)
			if readErr != nil {
				if readErr == io.EOF {
					t.log.Infof("remotetrack: track ended %s ", t.track.ID())
					t.rtppool.PutPayload(buffer)
					return
				}

				t.log.Tracef("remotetrack: read error: %s", readErr.Error())
				t.rtppool.PutPayload(buffer)
				continue
			}

			// could be read deadline reached
			if n == 0 {
				t.rtppool.PutPayload(buffer)
				continue
			}

			p := t.rtppool.GetPacket()

			if err := t.unmarshal((*buffer)[:n], p); err != nil {
				t.log.Errorf("remotetrack: unmarshal error: %s", err.Error())
				t.rtppool.PutPayload(buffer)
				t.rtppool.PutPacket(p)
				continue
			}

			if !t.IsRelay() {
				go t.updateStats()
			}

			t.onRead(attrs, p)

			t.rtppool.PutPayload(buffer)
			t.rtppool.PutPacket(p)
		}
	}
}

func (t *remoteTrack) unmarshal(buf []byte, p *rtp.Packet) error {
	n, err := p.Header.Unmarshal(buf)
	if err != nil {
		return err
	}

	end := len(buf)
	if p.Header.Padding {
		p.PaddingSize = buf[end-1]
		end -= int(p.PaddingSize)
	}
	if end < n {
		return errors.New("remote track buffer too short")
	}

	p.Payload = buf[n:end]

	return nil
}

func (t *remoteTrack) updateStats() {
	s := t.statsGetter.Get(uint32(t.track.SSRC()))
	if s == nil {
		t.log.Warnf("remotetrack: stats not found for track: ", t.track.SSRC())
		return
	}

	// update the stats if the last update equal or more than 1 second
	latestUpdated := t.latestUpdatedTS.Load()
	if time.Since(time.Unix(0, int64(latestUpdated))).Seconds() <= 1 {
		return
	}

	if latestUpdated == 0 {
		t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))
		return
	}

	t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))

	if t.onStatsUpdated != nil {
		t.onStatsUpdated(s)
	}
}

func (t *remoteTrack) Track() IRemoteTrack {
	return t.track
}

func (t *remoteTrack) sendPLI() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// return if there is a pending PLI request
	maxGapSeconds := 250 * time.Millisecond
	requestGap := time.Since(t.lastPLIRequestTime)

	if requestGap < maxGapSeconds {
		return // ignore PLI request
	}

	t.lastPLIRequestTime = time.Now()

	go t.onPLI()
}

func (t *remoteTrack) enableIntervalPLI(interval time.Duration) {
	go func() {
		ctx, cancel := context.WithCancel(t.context)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.sendPLI()
			}
		}
	}()
}

func (t *remoteTrack) IsRelay() bool {
	_, ok := t.track.(*RelayTrack)
	return ok
}

func (t *remoteTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *remoteTrack) onEnded() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, f := range t.onEndedCallbacks {
		f()
	}
}

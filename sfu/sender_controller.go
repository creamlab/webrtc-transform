package sfu

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ducksouplab/ducksoup/env"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
)

type senderController struct {
	sync.Mutex
	ms             *mixerSlice
	fromPs         *peerServer
	toUserId       string
	ssrc           webrtc.SSRC
	kind           string
	sender         *webrtc.RTPSender
	ccEstimator    cc.BandwidthEstimator
	optimalBitrate uint64
	maxBitrate     uint64
	minBitrate     uint64
}

func newSenderController(pc *peerConn, ms *mixerSlice, sender *webrtc.RTPSender) *senderController {
	params := sender.GetParameters()
	kind := ms.output.Kind().String()
	ssrc := params.Encodings[0].SSRC

	return &senderController{
		ms:             ms,
		fromPs:         ms.fromPs,
		toUserId:       pc.userId,
		ssrc:           ssrc,
		kind:           kind,
		sender:         sender,
		ccEstimator:    pc.ccEstimator,
		optimalBitrate: ms.streamConfig.DefaultBitrate,
		maxBitrate:     ms.streamConfig.MaxBitrate,
		minBitrate:     ms.streamConfig.MinBitrate,
	}
}

func (sc *senderController) logError() *zerolog.Event {
	return sc.ms.logError().Str("context", "track").Str("toUser", sc.toUserId)
}

func (sc *senderController) logInfo() *zerolog.Event {
	return sc.ms.logInfo().Str("context", "track").Str("toUser", sc.toUserId)
}

func (sc *senderController) logDebug() *zerolog.Event {
	return sc.ms.logDebug().Str("context", "track").Str("toUser", sc.toUserId)
}

func (sc *senderController) capRate(in uint64) uint64 {
	if in > sc.maxBitrate {
		return sc.maxBitrate
	} else if in < sc.minBitrate {
		return sc.minBitrate
	}
	return in
}

// see https://datatracker.ietf.org/doc/html/draft-ietf-rmcat-gcc-02
// credits to https://github.com/jech/galene
func (sc *senderController) updateRateFromLoss(loss uint8) {
	sc.Lock()
	defer sc.Unlock()

	var newOptimalBitrate uint64
	prevOptimalBitrate := sc.optimalBitrate

	if loss < 5 {
		// loss < 0.02, multiply by 1.05
		newOptimalBitrate = prevOptimalBitrate * 269 / 256
	} else if loss > 25 {
		// loss > 0.1, multiply by (1 - loss/2)
		newOptimalBitrate = prevOptimalBitrate * (512 - uint64(loss)) / 512
		sc.logInfo().Int("value", int(loss)).Msg("loss_threshold_exceeded")
	} else {
		newOptimalBitrate = prevOptimalBitrate
	}

	sc.optimalBitrate = sc.capRate(newOptimalBitrate)
}

func (sc *senderController) loop() {
	go sc.loopReadRTCP(env.GCC)

	<-sc.ms.i.ready()
	if sc.kind == "video" && env.GCC {
		go sc.loopGCC()
	}
}

func (sc *senderController) loopGCC() {
	ticker := time.NewTicker(time.Duration(config.Common.EncoderControlPeriod) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-sc.ms.done():
			// TODO FIX it could happen that addSender have triggered this loop without the slice
			// to have actually started
			return
		case <-ticker.C:
			// update optimal video bitrate, leaving room for audio
			sc.Lock()
			sc.optimalBitrate = sc.capRate(uint64(sc.ccEstimator.GetTargetBitrate()) - config.Audio.MaxBitrate)
			sc.Unlock()
			sc.logDebug().Str("target", fmt.Sprintf("%v", sc.ccEstimator.GetTargetBitrate())).Str("stats", fmt.Sprintf("%v", sc.ccEstimator.GetStats())).Msg("gcc")
		}
	}
}
func (sc *senderController) loopReadRTCP(estimateWithGCC bool) {
	for {
		select {
		case <-sc.ms.done():
			return
		default:
			packets, _, err := sc.sender.ReadRTCP()
			if err != nil {
				if err != io.EOF && err != io.ErrClosedPipe {
					sc.logError().Err(err).Msg("read_sent_rtcp_failed")
					continue
				} else {
					return
				}
			}

			// if bandwidth estimation is done with TWCC+GCC, REMB won't work and RR are not needed
			if estimateWithGCC {
				// only forward PLIs
				for _, packet := range packets {
					switch packet.(type) {
					case *rtcp.PictureLossIndication:
						sc.ms.fromPs.pc.throttledPLIRequest("PLI from other peer")
					}
				}
			} else {
				for _, packet := range packets {
					switch rtcpPacket := packet.(type) {
					case *rtcp.PictureLossIndication:
						sc.ms.fromPs.pc.throttledPLIRequest("PLI from other peer")
					case *rtcp.ReceiverEstimatedMaximumBitrate:
						sc.logDebug().Msgf("%T %+v", packet, packet)
						// sc.updateRateFromREMB(uint64(rtcpPacket.Bitrate))
					case *rtcp.ReceiverReport:
						// sc.logDebug().Msgf("%T %+v", packet, packet)
						for _, r := range rtcpPacket.Reports {
							if r.SSRC == uint32(sc.ssrc) {
								sc.updateRateFromLoss(r.FractionLost)
							}
						}
					}
				}
			}

		}
	}
}

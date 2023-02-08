package sfu

import (
	"fmt"
	"sync"
	"time"

	"github.com/ducksouplab/ducksoup/helpers"
	"github.com/ducksouplab/ducksoup/store"
	"github.com/ducksouplab/ducksoup/types"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	DefaultSize     = 2
	MaxSize         = 8
	TracksPerPeer   = 2
	DefaultDuration = 30
	MaxDuration     = 1200
	Ending          = 15
)

// room holds all the resources of a given experiment, accepting an exact number of *size* attendees
type room struct {
	sync.RWMutex
	// guarded by mutex
	mixer               *mixer
	peerServerIndex     map[string]*peerServer // per user id
	connectedIndex      map[string]bool        // per user id, undefined: never connected, false: previously connected, true: connected
	joinedCountIndex    map[string]int         // per user id
	filesIndex          map[string][]string    // per user id, contains media file names
	running             bool
	deleted             bool
	createdAt           time.Time
	startedAt           time.Time
	inTracksReadyCount  int
	outTracksReadyCount int
	// channels (safe)
	waitForAllCh chan struct{}
	endCh        chan struct{}
	// other (written only during initialization)
	id           string // public id
	hid          string // internal id
	qualifiedId  string // prefixed by origin, used for indexing in roomStore
	namespace    string
	size         int
	duration     int
	neededTracks int
	ssrcs        []uint32
	// log
	logger zerolog.Logger
}

// private and not guarded by mutex locks, since called by other guarded methods

func qualifiedId(join types.JoinPayload) string {
	return join.Origin + "#" + join.RoomId
}

func newRoom(qualifiedId string, join types.JoinPayload) *room {
	// process duration
	duration := join.Duration
	if duration < 1 {
		duration = DefaultDuration
	} else if duration > MaxDuration {
		duration = MaxDuration
	}

	// process size
	size := join.Size
	if size < 1 {
		size = DefaultSize
	} else if size > MaxSize {
		size = MaxSize
	}

	// room initialized with one connected peer
	connectedIndex := make(map[string]bool)
	connectedIndex[join.UserId] = true
	joinedCountIndex := make(map[string]int)
	joinedCountIndex[join.UserId] = 1

	// create folder for logs
	helpers.EnsureDir("./data/" + join.Namespace)
	helpers.EnsureDir("./data/" + join.Namespace + "/logs") // used by x264 mutipass cache

	r := &room{
		peerServerIndex:     make(map[string]*peerServer),
		filesIndex:          make(map[string][]string),
		deleted:             false,
		connectedIndex:      connectedIndex,
		joinedCountIndex:    joinedCountIndex,
		waitForAllCh:        make(chan struct{}),
		endCh:               make(chan struct{}),
		createdAt:           time.Now(),
		inTracksReadyCount:  0,
		outTracksReadyCount: 0,
		qualifiedId:         qualifiedId,
		id:                  join.RoomId,
		hid:                 helpers.RandomHexString(12),
		namespace:           join.Namespace,
		size:                size,
		duration:            duration,
		neededTracks:        size * TracksPerPeer,
		ssrcs:               []uint32{},
	}
	r.mixer = newMixer(r)
	// log (call Run hook whenever logging)
	r.logger = log.With().
		Str("context", "room").
		Str("namespace", join.Namespace).
		Str("room", join.RoomId).
		Logger().
		Hook(r)

	return r
}

// Run: implement log Hook interface
func (r *room) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	sinceCreation := time.Since(r.createdAt).Round(time.Millisecond).String()
	e.Str("sinceCreation", sinceCreation)
	if !r.startedAt.IsZero() {
		sinceStart := time.Since(r.startedAt).Round(time.Millisecond).String()
		e.Str("sinceStart", sinceStart)
	}
}

func (r *room) userCount() int {
	return len(r.connectedIndex)
}

func (r *room) connectedUserCount() (count int) {
	return len(r.peerServerIndex)
}

// users are connected but some out tracks may still be in the process of
// being attached and (not) ready (yet)
func (r *room) allUsersConnected() bool {
	return r.connectedUserCount() == r.size
}

func (r *room) filePrefix(userId string) string {
	connectionCount := r.joinedCountForUser(userId)
	// caution: time reflects the moment the pipeline is initialized.
	// When pipeline is started, files are written to, but it's better
	// to rely on the time advertised by the OS (file properties)
	// if several files need to be synchronized
	return time.Now().Format("20060102-150405.000") +
		"-n-" + r.namespace +
		"-i-" + r.hid +
		"-r-" + r.id +
		"-u-" + userId +
		"-c-" + fmt.Sprint(connectionCount)
}

func (r *room) countdown() {
	// blocking "end" event and delete
	endTimer := time.NewTimer(time.Duration(r.duration) * time.Second)
	<-endTimer.C

	r.Lock()
	r.running = false
	r.Unlock()

	r.logger.Info().Msg("room_ended")
	// listened by peerServers, mixer, mixerTracks
	close(r.endCh)

	<-time.After(3000 * time.Millisecond)
	// most likely already deleted, see disconnectUser
	// except if room was empty before turning to r.running=false
	r.Lock()
	if !r.deleted {
		r.unguardedDelete()
	}
	r.Unlock()
}

// API read-write

func (r *room) incInTracksReadyCount(fromPs *peerServer, remoteTrack *webrtc.TrackRemote) {
	r.Lock()
	defer r.Unlock()

	if r.inTracksReadyCount == r.neededTracks {
		// reconnection case, then send start only once (check for "audio" for instance)
		if remoteTrack.Kind().String() == "audio" {
			go fromPs.ws.send("start")
		}
		return
	}

	r.inTracksReadyCount++
	r.logger.Info().Int("count", r.inTracksReadyCount).Msg("in_track_added_to_room")

	if r.inTracksReadyCount == r.neededTracks {
		// do start
		close(r.waitForAllCh)
		r.running = true
		r.logger.Info().Msg("room_started")
		r.startedAt = time.Now()
		// send start to all peers
		for _, ps := range r.peerServerIndex {
			go ps.ws.send("start")
		}
		go r.countdown()
		return
	}
}

func (r *room) addSSRC(ssrc uint32, kind string, userId string) {
	r.Lock()
	defer r.Unlock()

	r.ssrcs = append(r.ssrcs, ssrc)
	go store.AddToSSRCIndex(ssrc, kind, r.namespace, r.id, userId)
}

// returns true if signaling is needed
func (r *room) incOutTracksReadyCount() bool {
	r.Lock()
	defer r.Unlock()

	r.outTracksReadyCount++

	// all out tracks are ready
	if r.outTracksReadyCount == r.neededTracks {
		return true
	}
	// room with >= 3 users: after two users have disconnected and only one came back
	// trigger signaling (for an even number of out tracks since they go
	// by audio/video pairs)
	if r.running && (r.outTracksReadyCount%2 == 0) {
		return true
	}
	return false
}

func (r *room) decOutTracksReadyCount() {
	r.Lock()
	defer r.Unlock()

	r.outTracksReadyCount--
}

func (r *room) connectPeerServer(ps *peerServer) {
	r.Lock()
	defer r.Unlock()

	r.peerServerIndex[ps.userId] = ps
}

// should be called by another method that locked the room (mutex)
func (r *room) unguardedDelete() {
	r.deleted = true
	roomStoreSingleton.delete(r)
	r.logger.Info().Msg("room_deleted")
	// cleanup
	for _, ssrc := range r.ssrcs {
		store.RemoveFromSSRCIndex(ssrc)
	}
}

func (r *room) disconnectUser(userId string) {
	r.Lock()
	defer r.Unlock()

	// protects decrementing since RemovePeer maybe called several times for same user
	if r.connectedIndex[userId] {
		// remove user current connection details (=peerServer)
		delete(r.peerServerIndex, userId)
		// mark disconnected, but keep track of her
		r.connectedIndex[userId] = false
		go r.mixer.managedGlobalSignaling("user_disconnected", false)

		// users may have disconnected temporarily
		// delete only if is empty and not running
		if r.connectedUserCount() == 0 && !r.running && !r.deleted {
			r.unguardedDelete()
		}
	}
}

func (r *room) addFiles(userId string, files []string) {
	r.Lock()
	defer r.Unlock()

	r.filesIndex[userId] = append(r.filesIndex[userId], files...)
}

// API read

func (r *room) joinedCountForUser(userId string) int {
	r.RLock()
	defer r.RUnlock()

	return r.joinedCountIndex[userId]
}

func (r *room) files() map[string][]string {
	r.RLock()
	defer r.RUnlock()

	return r.filesIndex
}

func (r *room) endingDelay() (delay int) {
	r.RLock()
	defer r.RUnlock()

	elapsed := time.Since(r.startedAt)

	remaining := r.duration - int(elapsed.Seconds())
	delay = remaining - Ending
	if delay < 1 {
		delay = 1
	}
	return
}

// return false if an error ends the waiting
func (r *room) readRemoteTillAllReady(remoteTrack *webrtc.TrackRemote) bool {
	for {
		select {
		case <-r.waitForAllCh:
			// trial is over, no need to trigger signaling on every closing track
			return true
		default:
			_, _, err := remoteTrack.ReadRTP()
			if err != nil {
				r.logger.Error().Err(err).Msg("room readRemoteTillAllReady")
				return false
			}
		}
	}
}

func (r *room) runMixerSliceFromRemote(
	ps *peerServer,
	remoteTrack *webrtc.TrackRemote,
	receiver *webrtc.RTPReceiver,
) {
	// signal new peer and tracks
	r.incInTracksReadyCount(ps, remoteTrack)

	// prepare slice
	slice, err := newMixerSlice(ps, remoteTrack, receiver)
	if err != nil {
		r.logger.Error().Err(err).Msg("new_mixer_slice_failed")
	}
	// index to be searchable by track id
	r.mixer.indexMixerSlice(slice)
	// needed to relay control fx events between peer server and output track
	ps.setMixerSlice(remoteTrack.Kind().String(), slice)

	// wait for all peers to connect
	ok := r.readRemoteTillAllReady(remoteTrack)

	if ok {
		// trigger signaling if needed
		signalingNeeded := r.incOutTracksReadyCount()
		if signalingNeeded {
			// TODO FIX without this timeout, some tracks are not sent to peers,
			<-time.After(1000 * time.Millisecond)
			go r.mixer.managedGlobalSignaling("out_tracks_ready", true)
		}
		// blocking until room ends or user disconnects
		slice.loop()
		// track has ended
		r.mixer.removeMixerSlice(slice)
		r.decOutTracksReadyCount()
	}
}

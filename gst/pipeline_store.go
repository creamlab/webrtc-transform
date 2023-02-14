package gst

import (
	"sync"

	"github.com/rs/zerolog/log"
)

var (
	// sfu package exposed singleton
	pipelineStoreSingleton *pipelineStore
)

type pipelineStore struct {
	sync.Mutex
	index map[string]*Pipeline
}

func init() {
	pipelineStoreSingleton = newPipelinetore()
}

func newPipelinetore() *pipelineStore {
	return &pipelineStore{sync.Mutex{}, make(map[string]*Pipeline)}
}

func (ps *pipelineStore) add(p *Pipeline) {
	ps.Lock()
	defer ps.Unlock()

	ps.index[p.id] = p
}

func (ps *pipelineStore) find(id string) (p *Pipeline, ok bool) {
	ps.Lock()
	defer ps.Unlock()

	p, ok = ps.index[id]
	return
}

func (ps *pipelineStore) delete(id string) {
	ps.Lock()
	defer ps.Unlock()

	p, ok := ps.index[id]
	if ok {
		log.Info().Str("context", "pipeline").Str("namespace", p.join.Namespace).Str("interaction", p.join.InteractionName).Str("user", p.join.UserId).Str("pipeline", id).Msg("pipeline_deleted")
	}

	delete(ps.index, id)
}

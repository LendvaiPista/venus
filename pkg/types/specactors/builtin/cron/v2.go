package cron

import (
	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/venus/pkg/types/specactors/adt"

	cron2 "github.com/filecoin-project/specs-actors/v2/actors/builtin/cron"
)

var _ State = (*state2)(nil)

func load2(store adt.Store, root cid.Cid) (State, error) {
	out := state2{store: store}
	err := store.Get(store.Context(), root, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func make2(store adt.Store) (State, error) {
	out := state2{store: store}
	out.State = *cron2.ConstructState(cron2.BuiltInEntries())
	return &out, nil
}

type state2 struct {
	cron2.State
	store adt.Store
}

func (s *state2) GetState() interface{} {
	return &s.State
}

package paychmgr

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/filecoin-project/venus/pkg/repo"
	"golang.org/x/xerrors"

	"github.com/google/uuid"

	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	dsq "github.com/ipfs/go-datastore/query"

	"github.com/filecoin-project/go-address"
	fbig "github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/venus/pkg/types/specactors/builtin/paych"
)

var ErrChannelNotTracked = errors.New("channel not tracked")

type Store struct {
	ds datastore.Datastore
}

// for test
func NewStore(ds repo.Datastore) *Store {
	dsTmp := namespace.Wrap(ds, datastore.NewKey("/paych/"))
	return &Store{
		ds: dsTmp,
	}
}

const (
	DirInbound  = 1
	DirOutbound = 2
)

const (
	dsKeyChannelInfo = "ChannelInfo"
	dsKeyMsgCid      = "MsgCid"
)

type VoucherInfo struct {
	Voucher   *paych.SignedVoucher
	Proof     []byte // ignored
	Submitted bool
}

// ChannelInfo keeps track of information about a channel
type ChannelInfo struct {
	// ChannelID is a uuid set at channel creation
	ChannelID string
	// Channel address - may be nil if the channel hasn't been created yet
	Channel *address.Address
	// Control is the address of the local node
	Control address.Address
	// Target is the address of the remote node (on the other end of the channel)
	Target address.Address
	// Direction indicates if the channel is inbound (Control is the "to" address)
	// or outbound (Control is the "from" address)
	Direction uint64
	// Vouchers is a list of all vouchers sent on the channel
	Vouchers []*VoucherInfo
	// NextLane is the number of the next lane that should be used when the
	// client requests a new lane (eg to create a voucher for a new deal)
	NextLane uint64
	// Amount added to the channel.
	// Note: This amount is only used by GetPaych to keep track of how much
	// has locally been added to the channel. It should reflect the channel's
	// Balance on chain as long as all operations occur on the same datastore.
	Amount fbig.Int
	// PendingAmount is the amount that we're awaiting confirmation of
	PendingAmount fbig.Int
	// CreateMsg is the CID of a pending create message (while waiting for confirmation)
	CreateMsg *cid.Cid
	// AddFundsMsg is the CID of a pending add funds message (while waiting for confirmation)
	AddFundsMsg *cid.Cid
	// Settling indicates whether the channel has entered into the settling state
	Settling bool
}

func (ci *ChannelInfo) from() address.Address {
	if ci.Direction == DirOutbound {
		return ci.Control
	}
	return ci.Target
}

func (ci *ChannelInfo) to() address.Address {
	if ci.Direction == DirOutbound {
		return ci.Target
	}
	return ci.Control
}

// infoForVoucher gets the VoucherInfo for the given voucher.
// returns nil if the channel doesn't have the voucher.
func (ci *ChannelInfo) infoForVoucher(sv *paych.SignedVoucher) (*VoucherInfo, error) {
	for _, v := range ci.Vouchers {
		eq, err := cborutil.Equals(sv, v.Voucher)
		if err != nil {
			return nil, err
		}
		if eq {
			return v, nil
		}
	}
	return nil, nil
}

func (ci *ChannelInfo) hasVoucher(sv *paych.SignedVoucher) (bool, error) {
	vi, err := ci.infoForVoucher(sv)
	return vi != nil, err
}

// markVoucherSubmitted marks the voucher, and any vouchers of lower nonce
// in the same lane, as being submitted.
// Note: This method doesn't write anything to the store.
func (ci *ChannelInfo) markVoucherSubmitted(sv *paych.SignedVoucher) error {
	vi, err := ci.infoForVoucher(sv)
	if err != nil {
		return err
	}
	if vi == nil {
		return xerrors.Errorf("cannot submit voucher that has not been added to channel")
	}

	// Mark the voucher as submitted
	vi.Submitted = true

	// Mark lower-nonce vouchers in the same lane as submitted (lower-nonce
	// vouchers are superseded by the submitted voucher)
	for _, vi := range ci.Vouchers {
		if vi.Voucher.Lane == sv.Lane && vi.Voucher.Nonce < sv.Nonce {
			vi.Submitted = true
		}
	}

	return nil
}

// wasVoucherSubmitted returns true if the voucher has been submitted
func (ci *ChannelInfo) wasVoucherSubmitted(sv *paych.SignedVoucher) (bool, error) {
	vi, err := ci.infoForVoucher(sv)
	if err != nil {
		return false, err
	}
	if vi == nil {
		return false, xerrors.Errorf("cannot submit voucher that has not been added to channel")
	}
	return vi.Submitted, nil
}

// TrackChannel stores a channel, returning an error if the channel was already
// being tracked
func (ps *Store) TrackChannel(ci *ChannelInfo) (*ChannelInfo, error) {
	_, err := ps.ByAddress(*ci.Channel)
	switch err {
	default:
		return nil, err
	case nil:
		return nil, fmt.Errorf("already tracking channel: %s", ci.Channel)
	case ErrChannelNotTracked:
		err = ps.putChannelInfo(ci)
		if err != nil {
			return nil, err
		}

		return ps.ByAddress(*ci.Channel)
	}
}

// ListChannels returns the addresses of all channels that have been created
func (ps *Store) ListChannels() ([]address.Address, error) {
	cis, err := ps.findChans(func(ci *ChannelInfo) bool {
		return ci.Channel != nil
	}, 0)
	if err != nil {
		return nil, err
	}

	addrs := make([]address.Address, 0, len(cis))
	for _, ci := range cis {
		addrs = append(addrs, *ci.Channel)
	}

	return addrs, nil
}

// findChan finds a single channel using the given filter.
// If there isn't a channel that matches the filter, returns ErrChannelNotTracked
func (ps *Store) findChan(filter func(ci *ChannelInfo) bool) (*ChannelInfo, error) {
	cis, err := ps.findChans(filter, 1)
	if err != nil {
		return nil, err
	}

	if len(cis) == 0 {
		return nil, ErrChannelNotTracked
	}

	return &cis[0], err
}

// findChans loops over all channels, only including those that pass the filter.
// max is the maximum number of channels to return. Set to zero to return unlimited channels.
func (ps *Store) findChans(filter func(*ChannelInfo) bool, max int) ([]ChannelInfo, error) {
	res, err := ps.ds.Query(dsq.Query{Prefix: dsKeyChannelInfo})
	if err != nil {
		return nil, err
	}
	defer res.Close() //nolint:errcheck

	var stored ChannelInfo
	var matches []ChannelInfo

	for {
		res, ok := res.NextSync()
		if !ok {
			break
		}

		if res.Error != nil {
			return nil, err
		}

		ci, err := unmarshallChannelInfo(&stored, res.Value)
		if err != nil {
			return nil, err
		}

		if !filter(ci) {
			continue
		}

		matches = append(matches, *ci)

		// If we've reached the maximum number of matches, return.
		// Note that if max is zero we return an unlimited number of matches
		// because len(matches) will always be at least 1.
		if len(matches) == max {
			return matches, nil
		}
	}

	return matches, nil
}

// AllocateLane allocates a new lane for the given channel
func (ps *Store) AllocateLane(ch address.Address) (uint64, error) {
	ci, err := ps.ByAddress(ch)
	if err != nil {
		return 0, err
	}

	out := ci.NextLane
	ci.NextLane++

	return out, ps.putChannelInfo(ci)
}

// VouchersForPaych gets the vouchers for the given channel
func (ps *Store) VouchersForPaych(ch address.Address) ([]*VoucherInfo, error) {
	ci, err := ps.ByAddress(ch)
	if err != nil {
		return nil, err
	}

	return ci.Vouchers, nil
}

func (ps *Store) MarkVoucherSubmitted(ci *ChannelInfo, sv *paych.SignedVoucher) error {
	err := ci.markVoucherSubmitted(sv)
	if err != nil {
		return err
	}
	return ps.putChannelInfo(ci)
}

// ByAddress gets the channel that matches the given address
func (ps *Store) ByAddress(addr address.Address) (*ChannelInfo, error) {
	return ps.findChan(func(ci *ChannelInfo) bool {
		return ci.Channel != nil && *ci.Channel == addr
	})
}

// MsgInfo stores information about a create channel / add funds message
// that has been sent
type MsgInfo struct {
	// ChannelID links the message to a channel
	ChannelID string
	// MsgCid is the CID of the message
	MsgCid cid.Cid
	// Received indicates whether a response has been received
	Received bool
	// Err is the error received in the response
	Err string
}

// The datastore key used to identify the message
func dskeyForMsg(mcid cid.Cid) datastore.Key {
	return datastore.KeyWithNamespaces([]string{dsKeyMsgCid, mcid.String()})
}

// SaveNewMessage is called when a message is sent
func (ps *Store) SaveNewMessage(channelID string, mcid cid.Cid) error {
	k := dskeyForMsg(mcid)

	b, err := cborutil.Dump(&MsgInfo{ChannelID: channelID, MsgCid: mcid})
	if err != nil {
		return err
	}

	return ps.ds.Put(k, b)
}

// SaveMessageResult is called when the result of a message is received
func (ps *Store) SaveMessageResult(mcid cid.Cid, msgErr error) error {
	minfo, err := ps.GetMessage(mcid)
	if err != nil {
		return err
	}

	k := dskeyForMsg(mcid)
	minfo.Received = true
	if msgErr != nil {
		minfo.Err = msgErr.Error()
	}

	b, err := cborutil.Dump(minfo)
	if err != nil {
		return err
	}

	return ps.ds.Put(k, b)
}

// ByMessageCid gets the channel associated with a message
func (ps *Store) ByMessageCid(mcid cid.Cid) (*ChannelInfo, error) {
	minfo, err := ps.GetMessage(mcid)
	if err != nil {
		return nil, err
	}

	ci, err := ps.findChan(func(ci *ChannelInfo) bool {
		return ci.ChannelID == minfo.ChannelID
	})
	if err != nil {
		return nil, err
	}

	return ci, err
}

// GetMessage gets the message info for a given message CID
func (ps *Store) GetMessage(mcid cid.Cid) (*MsgInfo, error) {
	k := dskeyForMsg(mcid)

	val, err := ps.ds.Get(k)
	if err != nil {
		return nil, err
	}

	var minfo MsgInfo
	if err := minfo.UnmarshalCBOR(bytes.NewReader(val)); err != nil {
		return nil, err
	}

	return &minfo, nil
}

// OutboundActiveByFromTo looks for outbound channels that have not been
// settled, with the given from / to addresses
func (ps *Store) OutboundActiveByFromTo(from address.Address, to address.Address) (*ChannelInfo, error) {
	return ps.findChan(func(ci *ChannelInfo) bool {
		if ci.Direction != DirOutbound {
			return false
		}
		if ci.Settling {
			return false
		}
		return ci.Control == from && ci.Target == to
	})
}

// WithPendingAddFunds is used on startup to find channels for which a
// create channel or add funds message has been sent, but lotus shut down
// before the response was received.
func (ps *Store) WithPendingAddFunds() ([]ChannelInfo, error) {
	return ps.findChans(func(ci *ChannelInfo) bool {
		if ci.Direction != DirOutbound {
			return false
		}
		return ci.CreateMsg != nil || ci.AddFundsMsg != nil
	}, 0)
}

// ByChannelID gets channel info by channel ID
func (ps *Store) ByChannelID(channelID string) (*ChannelInfo, error) {
	var stored ChannelInfo

	res, err := ps.ds.Get(dskeyForChannel(channelID))
	if err != nil {
		if err == datastore.ErrNotFound {
			return nil, ErrChannelNotTracked
		}
		return nil, err
	}

	return unmarshallChannelInfo(&stored, res)
}

// CreateChannel creates an outbound channel for the given from / to
func (ps *Store) CreateChannel(from address.Address, to address.Address, createMsgCid cid.Cid, amt fbig.Int) (*ChannelInfo, error) {
	ci := &ChannelInfo{
		Direction:     DirOutbound,
		NextLane:      0,
		Control:       from,
		Target:        to,
		CreateMsg:     &createMsgCid,
		PendingAmount: amt,
	}

	// Save the new channel
	err := ps.putChannelInfo(ci)
	if err != nil {
		return nil, err
	}

	// Save a reference to the create message
	err = ps.SaveNewMessage(ci.ChannelID, createMsgCid)
	if err != nil {
		return nil, err
	}

	return ci, err
}

// RemoveChannel removes the channel with the given channel ID
func (ps *Store) RemoveChannel(channelID string) error {
	return ps.ds.Delete(dskeyForChannel(channelID))
}

// The datastore key used to identify the channel info
func dskeyForChannel(channelID string) datastore.Key {
	return datastore.KeyWithNamespaces([]string{dsKeyChannelInfo, channelID})
}

// putChannelInfo stores the channel info in the datastore
func (ps *Store) putChannelInfo(ci *ChannelInfo) error {
	if len(ci.ChannelID) == 0 {
		ci.ChannelID = uuid.New().String()
	}
	k := dskeyForChannel(ci.ChannelID)

	b, err := marshallChannelInfo(ci)
	if err != nil {
		return err
	}

	return ps.ds.Put(k, b)
}

// TODO: This is a hack to get around not being able to CBOR marshall a nil
// address.Address. It's been fixed in address.Address but we need to wait
// for the change to propagate to specs-actors before we can remove this hack.
var emptyAddr address.Address

func init() {
	addr, err := address.NewActorAddress([]byte("empty"))
	if err != nil {
		panic(err)
	}
	emptyAddr = addr
}

func marshallChannelInfo(ci *ChannelInfo) ([]byte, error) {
	// See note above about CBOR marshalling address.Address
	if ci.Channel == nil {
		ci.Channel = &emptyAddr
	}
	return cborutil.Dump(ci)
}

func unmarshallChannelInfo(stored *ChannelInfo, value []byte) (*ChannelInfo, error) {
	if err := stored.UnmarshalCBOR(bytes.NewReader(value)); err != nil {
		return nil, err
	}

	// See note above about CBOR marshalling address.Address
	if stored.Channel != nil && *stored.Channel == emptyAddr {
		stored.Channel = nil
	}

	return stored, nil
}

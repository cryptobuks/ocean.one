package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/MixinMessenger/bot-api-go-client"
	"github.com/MixinMessenger/go-number"
	"github.com/satori/go.uuid"
	"github.com/ugorji/go/codec"
	"ocean.one/config"
	"ocean.one/engine"
	"ocean.one/persistence"
)

const (
	BitcoinAssetId = "c6d0c728-2624-429b-8e0d-d9d19b6592fa"
	USDTAssetId    = "815b0b1a-2764-3736-8faa-42d694fa620a"
)

type Snapshot struct {
	SnapshotId string `json:"snapshot_id"`
	Amount     string `json:"amount"`
	Asset      struct {
		AssetId string `json:"asset_id"`
	} `json:"asset"`
	CreatedAt time.Time `json:"created_at"`

	TraceId    string `json:"trace_id"`
	UserId     string `json:"user_id"`
	OpponentId string `json:"opponent_id"`
	Data       string `json:"data"`
}

type OrderAction struct {
	S string
	A uuid.UUID
	P string
	O uuid.UUID
}

type Exchange struct {
	books map[string]*engine.Book
	codec codec.Handle
}

func NewExchange() *Exchange {
	return &Exchange{
		codec: new(codec.MsgpackHandle),
		books: make(map[string]*engine.Book),
	}
}

func (ex *Exchange) PollMixinNetwork(ctx context.Context) {
	checkpoint, limit := persistence.ReadLatestAction(ctx).UTC(), 500
	for {
		snapshots, err := ex.requestMixinNetwork(ctx, checkpoint, limit)
		if err != nil {
			log.Println("PollMixinNetwork ERROR", err)
			time.Sleep(1 * time.Second)
			continue
		}
		for _, s := range snapshots {
			log.Println(s)
			err := ex.processSnapshot(ctx, s)
			if err != nil {
				log.Println("PollMixinNetwork processSnapshot ERROR", err)
				break
			}
			checkpoint = s.CreatedAt
		}
		if len(snapshots) < limit {
			time.Sleep(1 * time.Second)
			continue
		}
	}
}

func (ex *Exchange) processSnapshot(ctx context.Context, s *Snapshot) error {
	if s.UserId != config.ClientId {
		return nil
	}
	if s.OpponentId == "" || s.TraceId == "" {
		return nil
	}
	if number.FromString(s.Amount).Exhausted() {
		return nil
	}

	action := ex.decryptOrderAction(ctx, s.Data)
	if action == nil {
		return ex.refund(ctx, s)
	}
	if action.A.String() == s.Asset.AssetId {
		return ex.refund(ctx, s)
	}
	if action.O.String() != uuid.Nil.String() {
		return persistence.CancelOrder(ctx, action.O.String())
	}

	amount := number.FromString(s.Amount).RoundFloor(8)
	price := number.FromString(action.P).RoundFloor(8)
	if price.Exhausted() {
		return ex.refund(ctx, s)
	}

	var quote, base string
	if action.S == engine.PageSideAsk {
		quote, base = action.A.String(), s.Asset.AssetId
	} else if action.S == engine.PageSideBid {
		quote, base = s.Asset.AssetId, action.A.String()
		amount = amount.Div(price)
	} else {
		return ex.refund(ctx, s)
	}
	if !ex.validateQuoteBasePair(quote, base) {
		return ex.refund(ctx, s)
	}

	return persistence.CreateOrder(ctx, s.OpponentId, s.TraceId, action.S, quote, base, amount, price, s.CreatedAt)
}

func (ex *Exchange) validateQuoteBasePair(quote, base string) bool {
	if quote != BitcoinAssetId && quote != USDTAssetId {
		return false
	}
	if quote == BitcoinAssetId && base == USDTAssetId {
		return false
	}
	return true
}

func (ex *Exchange) refund(ctx context.Context, s *Snapshot) error {
	return nil
}

func (ex *Exchange) decryptOrderAction(ctx context.Context, data string) *OrderAction {
	payload, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	var action OrderAction
	decoder := codec.NewDecoderBytes(payload, ex.codec)
	err = decoder.Decode(&action)
	if err != nil {
		return nil
	}
	return &action
}

func (ex *Exchange) requestMixinNetwork(ctx context.Context, checkpoint time.Time, limit int) ([]*Snapshot, error) {
	uri := fmt.Sprintf("/network/snapshots?offset=%s&order=ASC&limit=%d", checkpoint.Format(time.RFC3339Nano), limit)
	token, err := bot.SignAuthenticationToken(config.ClientId, config.SessionId, config.SessionKey, "GET", uri, "")
	if err != nil {
		return nil, err
	}
	body, err := bot.Request(ctx, "GET", uri, nil, token)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data  []*Snapshot `json:"data"`
		Error string      `json:"error"`
	}
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, errors.New(result.Error)
	}
	return result.Data, nil
}

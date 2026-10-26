package audiencevalid

import (
	"errors"
	"iter"
)

var ErrTooLarge = errors.New("audience too large for accounts rollup")

type Member struct {
	LeadID   int
	Snapshot map[string]any
}

type Signal struct{ Count, Weight int }

type Account struct {
	Key            string
	DecisionMakers int
}

type Result struct{ Accounts []Account }

func AccountKey(snapshot map[string]any) (key, company string) { return "", "" }

func Rollup(members iter.Seq2[Member, error], signals map[int]Signal, limit, offset, maxMembers int) (Result, error) {
	return Result{}, nil
}

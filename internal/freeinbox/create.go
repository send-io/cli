package freeinbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// TakenError is an address that cannot be created, with the relay's
// available suggestions.
type TakenError struct {
	Address     string
	Reason      string
	Suggestions []string
}

func (e *TakenError) Error() string {
	msg := fmt.Sprintf("%s is not available (%s)", e.Address, e.Reason)
	if len(e.Suggestions) > 0 {
		msg += "; available: " + strings.Join(e.Suggestions, ", ")
	}
	return msg
}

// CreateOptions select the address: Name and Org (normalised and checked
// with the relay), or an exact Address (a suggestion).
type CreateOptions struct {
	Name, Org, Address string
	// HoldToken is the check's hold token for Address (optional).
	HoldToken string
	// OnPoW is called before solving the proof of work.
	OnPoW func(difficulty int)
}

// CreateResult is a created inbox.
type CreateResult struct {
	Created
	RecoveryPhrase string `json:"recovery_phrase"`
	KeyPath        string `json:"key_path"`
}

// Create checks availability, solves the proof of work, registers the
// inbox and stores its key locally (key.json, state.json).
func Create(ctx context.Context, c *Client, st *Store, opt CreateOptions) (*CreateResult, error) {
	address, hold := strings.ToLower(strings.TrimSpace(opt.Address)), opt.HoldToken
	if address == "" {
		name, org := Normalize(opt.Name), Normalize(opt.Org)
		if err := CheckName(name); err != nil {
			return nil, err
		}
		if err := CheckOrg(org); err != nil {
			return nil, err
		}
		chk, err := c.Check(ctx, name, org)
		if err != nil {
			return nil, err
		}
		address, hold = chk.Address, chk.HoldToken
		if !chk.Available && !(chk.Reason == "org_owned" && orgOwner(st, chk.Address) != nil) {
			return nil, &TakenError{Address: chk.Address, Reason: chk.Reason, Suggestions: chk.Suggestions}
		}
	} else if _, _, _, ok := SplitAddress(address); !ok {
		return nil, fmt.Errorf("%q is not an address of the form name@org.pooldomain", opt.Address)
	}
	if st.Has(address) {
		return nil, fmt.Errorf("%s already has a local key in %s", address, st.KeyPath(address))
	}
	keys, err := NewKeys()
	if err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString
	req := CreateRequest{Address: address, SignPub: b64(keys.SignPub), BoxPub: b64(keys.BoxPub[:]), HoldToken: hold}
	if owner := orgOwner(st, address); owner != nil {
		req.OrgSig = b64(owner.SignOrg(address))
	}
	var created *Created
	for try := 0; ; try++ {
		ch, err := c.PoW(ctx)
		if err != nil {
			return nil, err
		}
		if opt.OnPoW != nil {
			opt.OnPoW(ch.Difficulty)
		}
		nonce, err := SolvePoW(ctx, ch.Challenge, ch.Difficulty)
		if err != nil {
			return nil, err
		}
		req.PoW = map[string]string{"challenge": ch.Challenge, "nonce": nonce}
		created, err = c.Create(ctx, req)
		var re *Error
		if errors.As(err, &re) && re.Code == "pow_invalid" && try == 0 {
			continue // expired or used: one more challenge
		}
		if errors.As(err, &re) && (re.Code == "taken" || re.Code == "org_owned" || re.Code == "reserved" || re.Code == "blocked") {
			if re.Suggestions == nil {
				re.Suggestions = []string{}
			}
			return nil, &TakenError{Address: address, Reason: re.Code, Suggestions: re.Suggestions}
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if created.Address == "" {
		created.Address = address
	}
	if created.CreatedAt == "" {
		created.CreatedAt = NowRFC3339()
	}
	kf := NewKeyFile(created.Address, keys, created.CreatedAt, NormalizeRelay(c.Relay))
	if err := st.SaveKey(kf); err != nil {
		return nil, fmt.Errorf("the inbox was created but its key could not be saved: %v; recovery phrase: %s", err, RecoveryPhrase(keys.Seed))
	}
	return &CreateResult{Created: *created, RecoveryPhrase: RecoveryPhrase(keys.Seed), KeyPath: st.KeyPath(created.Address)}, nil
}

// OwnsOrg reports whether a local inbox holds the org owner key for
// address's org label (so an org_owned address can still be created here).
func OwnsOrg(st *Store, address string) bool { return orgOwner(st, address) != nil }

// orgOwner returns the key of the earliest local inbox under the same
// org label and pool domain (the org owner, when it was created here).
func orgOwner(st *Store, address string) *Keys {
	_, org, pool, ok := SplitAddress(address)
	if !ok {
		return nil
	}
	list, _ := st.List()
	var best *KeyFile
	for _, k := range list {
		_, o, p, ok := SplitAddress(k.Address)
		if !ok || o != org || p != pool || strings.EqualFold(k.Address, address) {
			continue
		}
		if best == nil || k.CreatedAt < best.CreatedAt {
			best = k
		}
	}
	if best == nil {
		return nil
	}
	keys, err := best.Keys()
	if err != nil {
		return nil
	}
	return keys
}

// Recover stores the key of an existing inbox from its recovery phrase,
// after checking with the relay (a signed poll) that the key is right.
func Recover(ctx context.Context, c *Client, st *Store, phrase, address string) (*KeyFile, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if _, _, _, ok := SplitAddress(address); !ok {
		return nil, fmt.Errorf("%q is not an address of the form name@org.pooldomain", address)
	}
	seed, err := ParsePhrase(phrase)
	if err != nil {
		return nil, err
	}
	keys, err := KeysFromSeed(seed)
	if err != nil {
		return nil, err
	}
	if _, err := c.Poll(ctx, address, keys, 0, 0, 1); err != nil {
		var re *Error
		if errors.As(err, &re) && re.Status >= 400 && re.Status < 500 {
			return nil, fmt.Errorf("the relay did not accept this phrase for %s (%s)", address, re.Error())
		}
		return nil, err
	}
	kf := NewKeyFile(address, keys, NowRFC3339(), NormalizeRelay(c.Relay))
	if old, err := st.LoadKey(address); err == nil && old.CreatedAt != "" {
		kf.CreatedAt = old.CreatedAt
	}
	if err := st.SaveKey(kf); err != nil {
		return nil, err
	}
	return kf, nil
}

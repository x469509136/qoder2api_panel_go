package accounts

// Candidates adapts the pool to the routing layer's interfaces.
func (p *Pool) Candidates() []PoolCandidate {
	accts := p.Accounts()
	out := make([]PoolCandidate, 0, len(accts))
	for _, a := range accts {
		out = append(out, PoolCandidate{Account: a})
	}
	return out
}

// AccountLikes returns the routing-facing view of every account.
func (p *Pool) AccountLikes() []AccountLikeView {
	accts := p.Accounts()
	out := make([]AccountLikeView, 0, len(accts))
	for _, a := range accts {
		out = append(out, AccountLikeView{Account: a})
	}
	return out
}

// AccountLikeView adapts an Account for scope resolution.
type AccountLikeView struct{ Account *Account }

// UID implements the routing interface.
func (v AccountLikeView) UID() string { return v.Account.UID }

// Group implements the routing interface.
func (v AccountLikeView) Group() string {
	v.Account.mu.RLock()
	defer v.Account.mu.RUnlock()
	return v.Account.Group
}

// RealmOf implements the routing interface.
func (v AccountLikeView) RealmOf() string { return v.Account.Realm }

// PoolAsCandidates converts a candidate slice back to accounts.
func PoolAsCandidates(items []PoolCandidate) []*Account {
	out := make([]*Account, 0, len(items))
	for _, c := range items {
		out = append(out, c.Account)
	}
	return out
}

// Package qoder is a standalone, transport-agnostic client for Qoder's
// business APIs: device authorization, token refresh, credits/quota, plan,
// daily check-in and the campaign (rewards) platform.
//
// It deliberately knows nothing about gateways, proxies, routing, account
// pools, cooldowns or panels. Every call takes plain values in and returns
// plain values out, so the package can be dropped into any reverse proxy:
//
//	cli := qoder.New(qoder.Options{HTTP: qoder.NewHTTPClient(30 * time.Second)})
//	cfg := qoder.RealmConfigFor(qoder.RealmCN, nil)
//	cred := qoder.Credential{AccessToken: token}
//	res := cli.FetchCredits(ctx, cfg, cred)
//
// Errors are typed (see HTTPStatusError) so a host can map them onto its own
// retry and cooldown policy without parsing strings.
package qoder

# ADR-0003: Deny/ban = allowlist-based

**Status:** accepted (2026-08-15)
**Decisions ref:** D3

## Context
BDS has **no native ban feature** — verified empirically on the 1.26.43
binary (no `ban`/`pardon` strings; no ban command; no ban file; see
research/04). The operator wanted "ban users" as a v1 feature.

## Decision
Implement "deny" as **allowlist management** (the allowlist gate):
toggle `allow-list`, add/remove gamertags or XUIDs with live reload, plus
secondary per-IP host-firewall blocking and `kick` for immediate ejection.
The TUI must make identification easy: live roster, "recently connected"
list mined from the journal, and gamertag-only adding (XUID resolved by the
server on first join).

## Consequences
- + Uses only official, documented mechanisms.
- + Solves the operator's original blocker (not knowing players' XUIDs).
- - Deny is a positive list, not a negative one — onboarding requires the
  operator to keep `allow-list=false` until he flips it (his choice, his
  timing; the tool never auto-flips it).
- - IP blocking is host-level (ufw/iptables) and therefore
  distribution/firewall-specific — best-effort with clear feedback.

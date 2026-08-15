# ADR-0004: Open-source hygiene — no operator specifics in tracked content

**Status:** accepted (2026-08-15)
**Decisions ref:** D5

## Context
The project will be open-sourced. The working environment (operator IPs,
hostnames, paths, world names) must never leak into tracked content.

## Decision
- `.gitignore` aggressively excludes private material: `local/`, `*.local.*`,
  `.env`, build artifacts.
- Tracked docs refer to private values as "see `local/notes.md`".
- The tool learns its environment from config / CLI flags / autodiscovery;
  no operator value is hardcoded.
- Review checklist item before any commit: scan the diff for IPs,
  hostnames, usernames, absolute home paths.

## Consequences
- + Safe to publish the repo as-is.
- - Slight friction: two copies of some facts (tracked generic vs local
  specific) — accepted; the local copy is authoritative for the operator.

# Click fork of OffchainLabs/nitro

This fork exists so the Nitro image that `robinhood-prod-01` and `robinhood-01`
run is one **we** built and keep in our own registry, instead of pulling
`offchainlabs/nitro-node` from Docker Hub at deploy time.

Consumer: [`ansible-robinhood`](https://github.com/tradeonapex/ansible-robinhood)
(`robinhood_image`), driven from `rke2/<cluster>/inventory/host_vars/<host>`.

## Branch model

| Branch | What it is |
|---|---|
| `master` | The upstream tree, **frozen at fork time**. Never pushed to. |
| `click` | **Default branch.** Upstream tree + `.github/workflows/click-nitro-ecr.yml`, with upstream's own `.github/workflows/` removed. |

Two reasons `click` is the default and not a side branch:

1. `workflow_dispatch` only registers a workflow that lives on the default
   branch. A build workflow we cannot trigger by hand is useless.
2. Upstream has ~40 workflows, several on `push`. With Actions enabled on a
   fork, a push to `master` fires them on our runners. So `master` is frozen,
   our branch carries no upstream workflow, and every pre-existing workflow is
   additionally disabled through the API as a second layer:

   ```sh
   gh api repos/tradeonapex/nitro/actions/workflows -q '.workflows[] | "\(.id) \(.name)"'
   gh api -X PUT repos/tradeonapex/nitro/actions/workflows/<id>/disable
   ```

**Do not run `gh repo sync`.** It pushes to `master`, which is exactly the thing
the model above avoids.

## Building

Actions → *click · nitro-node → ECR* → Run workflow.

| Input | Note |
|---|---|
| `upstream_ref` | Tag, branch or SHA. A tag is the normal case. |
| `target` | `nitro-node` is what upstream publishes. `nitro-node-slim` drops the WAVM machines and the legacy image — plausible for us because we run non-validating (`--node.staker.enable=false`), but **unverified**. Do not put it in front of a chain without testing. |
| `runner` | Upstream builds this on 16 CPU / 64 GB. `lat-fra2-runners-rust` has no CPU limit and a 48Gi ceiling, the closest pool we have. |
| `push` / `force` | `push=false` proves a build without touching ECR. `force` overwrites a tag that already exists. |

The image tag is `<ref>-<short sha>`, which reproduces upstream's own naming:
tag `v3.12.1` is commit `70fa99a…`, so the build yields
`nitro-node:v3.12.1-70fa99a` — the same string we ran from Docker Hub, now under
`257394448504.dkr.ecr.eu-central-1.amazonaws.com`. A patched build has a
different sha, so it can never silently collide with an upstream-identical one.

### Required secrets

This fork sees no organisation secrets. `CI_AWS_ACCESS_KEY` and
`CI_AWS_SECRET_KEY` must be set as **repository** secrets (same names the
`gitops` repo uses). The workflow checks for them before the build rather than
after an hour of compute.

`DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` are optional, and only worth setting if
the cluster's egress IP starts collecting 429s — see below.

## What this does not buy

Building the image ourselves gives us control of the Go/Rust code and the
ability to carry patches. It does **not** make the build self-contained:

- the `machine-versions` stage runs `scripts/download-machine.sh` to fetch four
  WAVM consensus machines (`consensus-v50`, `v51`, `v51.1`, `v61`) over the
  network, and
- the Dockerfile has `FROM offchainlabs/nitro-node:v3.7.6-c0fe95e AS nitro-legacy`,
- plus the usual `debian`, `rust`, `golang` and `node` bases from Docker Hub.

If real supply-chain independence is the goal, those pieces have to be mirrored
too — the base images through `gitops/ecr-mirror/images.yaml`, the machines into
a bucket of ours.

## Bumping to a new upstream version

```sh
git fetch https://github.com/OffchainLabs/nitro.git --tags
git checkout click
git merge v3.13.0            # upstream's workflow files stay deleted; resolve
                             # "deleted by us" with `git rm` if it complains
git push origin click
```

Then dispatch the workflow with `upstream_ref: v3.13.0` and move
`robinhood_image` in the inventory once the node has been verified against it.
Never fast-forward `click` onto `master`, and never push a tag that points at an
upstream-tree commit.

## This is a public fork running on self-hosted runners

A fork of a public repository cannot be made private, and the build runs on our
own nodes inside lat-fra2. The two facts together are the standing hazard here:
a workflow that triggers on `pull_request` would let anyone on the internet run
code on a cluster node.

What keeps that shut:

- the build workflow is **`workflow_dispatch` only**, which requires write
  access to this repository;
- `fork-pr-contributor-approval` is set to `all_external_contributors`, so no
  workflow runs from any outside PR without a maintainer approving it:

  ```sh
  gh api repos/tradeonapex/nitro/actions/permissions/fork-pr-contributor-approval
  ```

Do not add `pull_request`, `pull_request_target` or `issue_comment` triggers to
a workflow in this repository. If PR validation is ever wanted here, it belongs
on GitHub-hosted runners, which are free for a public repository.

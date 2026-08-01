# Deployment

A push to `main` builds this repository's image and deploys it to the Nomad
cluster. `deploy.yml` is the whole of it.

## Why a self-hosted runner

The cluster is a single Raspberry Pi reachable only over Tailscale, at
`100.116.81.88`. A GitHub-hosted runner is not on that tailnet, so it would
have to join one (a Tailscale OAuth secret) and be handed the Nomad mTLS client
certificates as secrets before it could deploy anything.

A runner on the Pi itself needs neither. It also removes the registry from the
path entirely: the Pi is arm64, so it builds the image natively and loads it
straight into the Docker daemon that Nomad will run it from. Nothing has to be
pushed anywhere, and no credential leaves the machine.

The tradeoff is that the image exists only on that one host. A second client
added to the cluster could not run the job until the image was built there too.
If that day comes, `packs/museum/scripts/publish-image.sh` in the IaC
repository is the registry route — GitHub Actions' built-in `GITHUB_TOKEN`
carries `packages: write`, so pushing to GHCR from CI needs no personal token.

## Where the job spec lives

Not here. It is a Nomad Pack in
[artback/iac_jonathan](https://github.com/artback/iac_jonathan) under
`packs/museum`, alongside the sixteen other services on the same Pi. The
workflow checks that repository out on the runner at deploy time and runs
`nomad-pack` from it.

This repository holds only the workflow. The split keeps every pack in one
place while still letting a change to the application publish itself.

## One-time setup

From the IaC repository, on your Mac:

```bash
./packs/museum/scripts/setup-runner.sh
```

That provisions the Pi: it installs `nomad-pack`, clones the IaC repository to
`/opt/iac_jonathan`, and copies two things that are deliberately not in any
repository —

| Path on the Pi | What it is |
|---|---|
| `/opt/museum/museum-kalmar.pkrvars.hcl` | the vars file, holding the database and MinIO passwords |
| `/opt/museum/certs/` | the Nomad mTLS client certificates |

Both are `chmod 600`. They are copied rather than committed or stored as GitHub
secrets because the runner is already inside the network that trusts them —
putting them in GitHub's secret store would widen their blast radius for no
gain.

The script then prints the runner registration steps, which need a token only
you can generate from the repository's settings.

## Manual deploys

The workflow can be run by hand from the Actions tab, including in seed mode —
which detaches the MinIO bucket notification and stops the enricher. That
matters when loading data in bulk: the enricher re-geocodes every record an
event names, so seeding with the notification attached would queue hundreds of
thousands of requests against a geocoder that permits one per second.

To deploy from your Mac instead, bypassing CI:

```bash
nomad-pack run packs/museum -f vars/museum-kalmar.pkrvars.hcl
```

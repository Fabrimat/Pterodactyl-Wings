[![Logo Image](https://cdn.pterodactyl.io/logos/new/pterodactyl_logo.png)](https://pterodactyl.io)

![Discord](https://img.shields.io/discord/122900397965705216?label=Discord&logo=Discord&logoColor=white)
![GitHub Releases](https://img.shields.io/github/downloads/pterodactyl/wings/latest/total)
[![Go Report Card](https://goreportcard.com/badge/github.com/pterodactyl/wings)](https://goreportcard.com/report/github.com/pterodactyl/wings)

# Pterodactyl Wings

Wings is Pterodactyl's server control plane, built for the rapidly changing gaming industry and designed to be
highly performant and secure. Wings provides an HTTP API allowing you to interface directly with running server
instances, fetch server logs, generate backups, and control all aspects of the server lifecycle.

In addition, Wings ships with a built-in SFTP server allowing your system to remain free of Pterodactyl specific
dependencies, and allowing users to authenticate with the same credentials they would normally use to access the Panel.

## Sponsors

I would like to extend my sincere thanks to the following sponsors for helping fund Pterodactyl's development.
[Interested in becoming a sponsor?](https://github.com/sponsors/pterodactyl)

| Company                                                                           | About                                                                                                                                                                                                                                           |
|-----------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| [**Buildurly**](https://buildurly.com/)                                           | Buildurly is a hardware procurement company. They deliver tailored, enterprise-grade hardware solutions designed around your unique needs. From sourcing to delivery, Buildurly's white-glove service ensures a seamless, worry-free, professional experience.                                                                                                                                          |
| [**Hosturly**](https://hosturly.com/)                                             | Hosturly is an enterprise hosting provider. They provide cost-effective, high-performance, and reliable services, including VPS, Web, Dedicated, and Colocation.                                                                                |
| [**indifferent broccoli**](https://indifferentbroccoli.com/)                      | indifferent broccoli is a game server hosting and rental company. With them, you get top-notch computer power for your gaming sessions. They destroy lag, latency, and complexity--letting you focus on the fun stuff.                         |
| [**Infraly, LLC**](https://infraly.co/)                                           | Infraly is an infrastructure company powering the next generation of online services. Through their brands, Infraly delivers cutting-edge solutions across multiple markets. Their vertically integrated approach provides unmatched performance, scalability, and reliability, giving our customers full control.                                                                                     |
| [**MineStrator**](https://minestrator.com/)                                       | MineStrator is a game server hosting provider. Looking for the most high-end French hosting company for your Minecraft server? More than 24,000 members on our Discord trust us. Give us a try!                                                |
| [**Physgun**](https://physgun.com/)                                               | Physgun is a game server hosting provider. Most providers rent rack space and rebrand a panel. At Physgun, they engineer the performance, write the features, and staff the support. Physgun truly is game hosting perfected!                   |
| [**WISP**](https://wisp.gg/)                                                      | WISP is an industry-leading SaaS platform for game server management, designed for hosting companies, gaming organizations, and enthusiasts. WISP combines modern, intuitive interfaces with powerful tools, making server deployment and administration seamless, scalable, and efficient.     

## Documentation

* [Panel Documentation](https://pterodactyl.io/panel/1.0/getting_started.html)
* [Wings Documentation](https://pterodactyl.io/wings/1.0/installing.html)
* [Community Guides](https://pterodactyl.io/community/about.html)
* Or, get additional help [via Discord](https://discord.gg/pterodactyl)

## Installing this fork

This fork publishes no GitHub releases, so there is no prebuilt binary to
download. Build it from source and install the binary in place of stock
Wings.

### Get the source

```bash
git clone https://github.com/Fabrimat/Pterodactyl-Wings.git
cd Pterodactyl-Wings
```

### Node dependencies this fork adds

Docker is already required by upstream Wings. On top of that, the borg
backup adapter needs:

* borg, version 1.2 or newer but older than 2.0 (floor: `import-tar` and
  `--upload-ratelimit`; ceiling: 2.0 renamed `init` to `repo-create` and
  changed the encryption mode names). Wings resolves `borg` from `PATH` and
  fails with an explicit error if it is missing.
* An ssh client, because remote repositories run over `BORG_RSH` with
  `StrictHostKeyChecking=yes` and `BatchMode=yes`.

On Debian/Ubuntu:

```bash
apt install borgbackup openssh-client
```

If the repository lives on a separate host, that host needs borg installed
too. See "Preparing an ssh:// repository host" in the Panel's
[`BACKUPS.md`](https://github.com/Fabrimat/Pterodactyl-Panel/blob/1.0-develop/BACKUPS.md#preparing-an-ssh-repository-host)
for setting up the `borg` user, the forced command and the ssh key.

### Build

With Go 1.24 installed on the machine doing the build:

```bash
make build
```

This produces `build/wings_linux_amd64` and `build/wings_linux_arm64`.

Without Go on the host, which is the usual case on a node, build in a
container instead:

```bash
docker run --rm -v "$(pwd)":/src -v wings-gomod:/go/pkg/mod -w /src \
  -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 golang:1.24 \
  go build -buildvcs=false -trimpath \
  -ldflags='-s -w -X github.com/pterodactyl/wings/system.Version=borg-<short sha>' \
  -o build/wings_linux_amd64 github.com/pterodactyl/wings
```

`-buildvcs=false` is required because the container runs as root over a
checkout owned by another user; without it the build fails with "detected
dubious ownership in repository at '/src'". The version string must not
start with `v`: the `wings version` command prepends one itself, so a
version starting with `v` prints as `wings vvborg-<short sha>`.

### Install the binary and restart

Keep the current binary around for rollback, then replace it:

```bash
cp /usr/local/bin/wings /usr/local/bin/wings.bak
install -m 0755 -o root -g root build/wings_linux_amd64 /usr/local/bin/wings
systemctl restart wings
```

The systemd unit and `config.yml` are unchanged by this fork; see upstream's
[Wings Documentation](https://pterodactyl.io/wings/1.0/installing.html) for
those.

### Verify

```bash
wings version
```

This prints the version prefixed with `v`, for example
`wings vborg-<short sha>`.

Upgrade Wings on every node before upgrading the Panel: this fork's Panel
checks the `features` array Wings reports on `GET /api/system` and refuses
borg operations against a node whose daemon does not advertise them.

## Reporting Issues

Please use the [pterodactyl/panel](https://github.com/pterodactyl/panel) repository to report any issues or make
feature requests for Wings. In addition, the [security policy](https://github.com/pterodactyl/panel/security/policy) listed
within that repository also applies to Wings.

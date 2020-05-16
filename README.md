# pcmd
`pcmd` is a small utility to make SSH `ProxyCommand` more powerful. It might be
useful any time you have a `ProxyCommand` that involves a lengthy or expensive
set-up or tear-down procedure.

The primary use case of `pcmd` is to wrap a `ProxyCommand` that might need to
perform non-trivial set-up or tear-down operations. For example, one might have
a `ProxyCommand` that provisions and deletes a cloud VPS for disposable
development environments, accessible via `ssh my.temporary.host`. For such a use
case, it is essential to a) not duplicate work and b) ensure that resources get
cleaned up. Without `pcmd` it can be difficult to reliably meet those
requirements.

Complex proxy scripts such as those pose a few challenges:

1) The shutdown procedure might be rather lengthy. On some versions and
   configurations of SSH (like on macOS), as soon as the SSH connection is
   closed, a hangup signal is sent to the `ProxyCommand`, preventing any cleanup
   from occurring. That is bad because then you get billed for a VPS that you
   wanted to be disposable!
2) Logging. It's nice to be able to see logs to stderr while waiting for the VPS
   to come up. Once the SSH connection is closed, though, it's very annoying if
   logs to stderr continue to get dumped into your terminal. That is what would
   happen if you resolved issue 1) by trapping and ignoring signals.
3) Connection sharing and concurrency control. If you initiate multiple SSH
   connections, you probably want to re-use an existing VPS when possible. You
   might think: doesn't SSH let you do this? It does! You can set `ControlMaster
   auto` and specify a `ControlPath` in `~/.ssh/config`, and SSH
   opportunistically re-uses existing connections, skipping the `ProxyCommand`
   on subsequent SSH invocations. There's a small problem, though: what if the
   first connection is still in the "set-up" phase and hasn't established a
   connection? It turns out that SSH doesn't do any blocking/locking, and, in
   that case, it executes the `ProxyCommand` twice.

`pcmd` solves all of those problems:

1) `pcmd` shields the underlying `ProxyCommand` from interrupt and hangup
   signals. When a signal is received, pcmd closes stdio (to unblock any hung
   proxying) and starts a grace period timer (default 5 minutes). During this
   grace period, the `ProxyCommand` can perform any tear-down it needs. If the
   command hasn't exited by the end of the grace period, `pcmd` sends a kill
   signal.
2) `pcmd` tees stderr of the `ProxyCommand` to a log file as well as the
   terminal. That way, while waiting for the connection, you can see any
   relevant logs. Once the connection is closed, `pcmd` continues copying stderr
   of the command to the log file (so you can inspect it later) but stops
   copying stderr to the terminal. That way, you don't get logs dumped into the
   terminal when you move on to something else.
3) `pcmd` (if instructed with the `-lock` flag) uses file locking (via flock) to
   ensure only one copy of your `ProxyCommand` runs at once. If you want to
   share connections using `ControlMaster`, you can specify `-wait-for-master`.
   If `pcmd` doesn't have a lock, it blocks waiting for the control master to
   come up. As a nicety, it also tails the log file mentioned in 2) to stderr so
   you can monitor the progress of a lengthy set-up.

## Compatibility and dependencies
`pcmd` is cross-compiled to many targets, but has only been tested on macOS and
Linux (Ubuntu). Additionally, `pcmd` requires `tail` and, depending on
configuration, `ssh` to be available. These are extremely common.

## Install

### Pre-compiled binaries
`pcmd` is available for download from the [releases
page](https://github.com/andrewhamon/pcmd/releases/latest). You can also use the
following snippet to install the latest release of `pcmd`, adjusting the values
accordingly for your environment:

```sh
# Possible options: darwin,linux,freebsd,openbsd,netbsd
# darwin == macOS
OS=darwin

# Possible options: amd64,386,arm,arm64(linux only)
ARCH=amd64

TARGET=pcmd-$OS-$ARCH

# Download and unzip
curl -OL https://github.com/andrewhamon/pcmd/releases/latest/download/$TARGET.zip
unzip $TARGET.zip

# Copy to somewhere on your $PATH
# replace ~/bin with something appropriate for your environment
cp $TARGET/pcmd ~/bin
```

### Go install
If you have Go installed, you can also install `pcmd` using `go get`:

```sh
go get github.com/andrewhamon/pcmd
```

## Examples
The easiest way to get started is to wrap your original `ProxyCommand` with
`pcmd` in your SSH config. For example, if your SSH config looks like this:

```
Host some-host
        ProxyCommand original-proxy-command --original-arg foobar
```

You can prefix `original-proxy-command` and its arguments with `pcmd`:

```
Host some-host
        ProxyCommand pcmd original-proxy-command --original-arg foobar
```

This will continue to proxy as before but also ensures `original-proxy-command`
has adequate time after the connection closes to perform cleanup.

### Complete example: on-demand VPS with DigitalOcean
Included in this repo is an example bash script that uses `doctl` to create a
VPS on the fly and proxy to it. The following is is the resulting SSH config,
configured for connection sharing with `ControlMaster`. To use, you will need to
ensure that the `ondemand-proxy` script is downloaded and available in your
`$PATH` and that you have installed `doctl`.

```
Host ondemand.dev
        User root
        ProxyCommand pcmd --wait-for-master -r %r -h %h -p %p ondemand-proxy %h
        ControlMaster auto
        ControlPath ~/.ssh/%r@%h:%p.sock

        # Keep the connection alive for 5 minutes after the last connection
        # is closed. This lets you quickly re-connect without waiting for
        # tear-down and setup
        ControlPersist 300

        # Even when reviving a snapshot, DigitalOcean seems to generate a new
        # key, which will then cause scary warnings.
        StrictHostKeyChecking no
        UserKnownHostsFile=/dev/null
```

![pcmd in action](/pcmd-demo.svg?raw=true&sanitize=true)

### Configurable grace period
You can configure the grace period (default 5 minutes) with the `-grace-period`
flag. For example, to set the grace period to 10 minutes:

```
pcmd -grace-period 600 original-proxy-command --original-arg foobar
```

The grace period begins only once the SSH connection is closed.

### Preventing concurrent `ProxyCommand` invocations
If you add the `-lock` flag, `pcmd` can ensure that only one copy of
`original-proxy-command` runs at a time. To do so, `pcmd` needs to know the SSH
remote user and host, which it uses to form a unique key for locking. For
example:

```
pcmd -lock -r %r -h %h original-proxy-command --original-arg foobar
```

`%r` and `%h` are expanded by SSH automatically. See TOKENS in SSH_CONFIG(5) for
more details.

### Connection sharing
SSH provides a native mechanism for connection sharing via the `ControlMaster`
and `ControlPath` configuration options, which allows you to share a single
connection for multiple SSH sessions (see SSH_CONFIG(5) for more details). One
issue with the native mechanism, however, is that SSH doesn't do any concurrency
control. That means if you have a lengthy set-up in your proxy command, and then
try to establish two concurrent connections, SSH does not block one connection
waiting for a master. Instead, it runs both `ProxyCommand`s at the same time.

`pcmd` can help with this, by doing two things:

- Establishing a lock (using the `flock` system call) to ensure only one version
  of your `ProxyCommand` is running at a time.
- Waits for the `ControlMaster` to come up, if the lock can not be acquired.
  `pcmd` checks the status of the control master using `ssh user@host -O check`.

To set up connection sharing, add the `-wait-for-master` flag, along with the
`-r` and `-h` flags. For example:

```
Host some-host
        ProxyCommand pcmd -wait-for-master -r %r -h %h original-proxy-command --original-arg foobar
        ControlMaster auto
        ControlPath ~/.ssh/ssh-%r@%h:%p

        # Setting ControlPersist to a non-zero value ensures that SSH keeps the
        # master connection running in the background, rather than blocking
        # the first connection until all child connections also complete. If you
        # want to keep your connection alive in the background for longer, to
        # allow fast resumption, set this to a higher value (in number of
        # seconds)
        ControlPersist 1
```

The above config will allow only a single master connection, and make any
subsequent connections wait for the master to come up before continuing.
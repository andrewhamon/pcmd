# pcmd
`pcmd` is a small utility to make SSH `ProxyCommand` more powerful.

The primary use case of `pcmd` is to wrap a `ProxyCommand` that might need to
perform non-trivial setup or teardown operations. For example, one might have a
`ProxyCommand` that provisions and deletes remote hosts for disposable
development environments, accessible via `ssh my.temporary.host`. For such a use
case, it is essential to a) not duplicate work and b) ensure that resources get
cleaned up. Without `pcmd` it can be difficult to reliably meet those
requirements.

`pcmd` might be useful any time you have a `ProxyCommand` that involves a
lengthy or expensive setup or teardown procedure.

Specifically, `pcmd` provides the following features:

- Shields the original `ProxyCommand` from interrupt and hangup signals (SIGINT
  and SIGHUP), providing a grace period (default 5 minutes) during which the
  command can perform any necessary teardown or cleanup once the SSH connection
  is closed.
- Directs stderr to the terminal and a file during setup, but then directs
  stderr only to a file once the session is over. Teardown logs, therefore,
  won't be dumped into your terminal after the SSH session completes.
- Optionally provides locking, to ensure that no two versions of `ProxyCommand`
  are running at the same time.
- Optionally integrates with SSH `ControlMaster` config, for effortless
  connection sharing (See the Connection Sharing section below).

## Install
TBD

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

### Connection Sharing
SSH provides a native mechanism for connection sharing via the `ControlMaster`
and `ControlPath` configuration options, which allows you to share a single
connection for multiple SSH sessions (see SSH_CONFIG(5) for more details). One
issue with the native mechanism, however, is that SSH doesn't do any concurrency
control. That means if you have a lengthy setup in your proxy command, and then
try to establish two concurrent connections, SSH does not block one connection
waiting for a master. Instead, it runs both `ProxyCommand`s at the same time.

`pcmd` can help with this, by doing two things:

- Establishing a lock (using the `flock` syscall) to ensure only one version of
  your `ProxyCommand` is running at a time.
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
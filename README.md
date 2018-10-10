# Booking.com carbonapi

Main files for carbonapi and carbonzipper.

## Build

To build the `carbonapi` and `carbonzipper` binaries, run:
```
$ make
```
We use Go modules for dependency management, but vendor the dependencies to
ensure repeatable builds across different staging hosts that may not have the
same packages cached locally. Use
```
$ make vendor
```
to update the vendored depenencies after making changes.

## What? Why?

The main development of carbonapi happens in the Booking.com Github repo:

    https://www.github.com/bookingcom/carbonapi

However, we still need to be able to roll out code using git-deploy, derp and
Gitlab internally.

Our solution to this, at the moment, is to treat almost everything in the
Github repo as library code we pull in, and only keep `main` package files in
the Gitlab repo that we build code from. This way we avoid the headache of
keeping the Github and Gitlab repos in sync, and very hopefully avoid having
the two drift apart.

This also has the upshot that if we want to do Booking.com specific things,
like talk to sysctl, use rosters, or send events of some kind, we can do that
in these `main` packages without the wider world being any wiser.

## Updating dependencies

Run
```
$ go get -u github.com/bookingcom/carbonapi
$ go mod vendor
```
and then check-in and commit the results. This requires Go modules.

## Modules

Read about Go modules here:

    https://github.com/golang/go/wiki/Modules

Note that our staging hosts work in `GOPATH` and do _not_ have `GO111MODULE=on`
set.

# New Relic C SDK

This is the New Relic C SDK! If your application does not use other New Relic 
APM agent languages, you can use the C SDK to take advantage of New Relic's
monitoring capabilities and features to instrument a wide range of applications.

## Support

Do you have questions or are you experiencing unexpected behaviors with this 
Open Source Software? Please engage with us on the 
[New Relic Explorers Hub](https://discuss.newrelic.com/).

If you’re confident your issue is a bug, please follow our 
[bug reporting guidelines](needs_a_link.com) and open a GitHub Issue.

## Requirements

The C SDK works on 64-bit Linux operating systems with:

* gcc 4.8 or higher
* glibc 2.17 or higher
* Kernel version 2.6.26 or higher
* libpcre 8.20 or higher
* libpthread

Running unit tests requires cmake 2.8 or higher.

Compiling the New Relic daemon requires Go 1.4 or higher.

## Building the C SDK

If your system meets the requirements, building the C SDK and 
daemon should be as simple as:

```sh
make
```

This will create two files in this directory:

* `libnewrelic.a`: the static C SDK library, ready to link against.
* `newrelic-daemon`: the daemon binary, ready to run.

You can start the daemon in the foreground with:

```sh
./newrelic-daemon -f --logfile stdout --loglevel debug
```

Then you can invoke your instrumented application.  Your application,
which makes C SDK API calls, reports data to the daemon over a socket;
in turn, the daemon reports the data to New Relic.

## Using the SDK

Documentation is available at [docs.newrelic.com](docs.newrelic.com). 
API usage information can be found in libnewrelic.h. Other header files
are internal to the agent, and their stability is not guaranteed. Working 
examples are available in the [examples](../examples/) directory.

## Tests

To compile and run the unit tests:

```sh
make run_tests
```

Or, just to compile them:

```sh
make tests
```

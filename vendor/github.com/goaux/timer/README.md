# timer
The `timer` module provides a context-aware Sleep function for Go applications.

[![Go Reference](https://pkg.go.dev/badge/github.com/goaux/timer.svg)](https://pkg.go.dev/github.com/goaux/timer)
[![Go Report Card](https://goreportcard.com/badge/github.com/goaux/timer)](https://goreportcard.com/report/github.com/goaux/timer)

## Installation

To install the `timer` module, use the following command:

    go get github.com/goaux/timer

## Usage

Here's a basic example of how to use the `Sleep` function:

    package main

    import (
        "fmt"
        "os"
        "os/signal"
        "time"

        "github.com/goaux/timer"
    )

    func main() {
        // Create a context that will be canceled when SIGINT is received
        ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
        defer stop()

        fmt.Println("Starting sleep for 10 seconds. Press Ctrl+C to interrupt.")

        // Try to sleep for 10 seconds
        err := timer.SleepCause(ctx, 10*time.Second)

        if err != nil {
            fmt.Printf("Sleep interrupted: %v\n", err)
        } else {
            fmt.Println("Sleep completed successfully")
        }

        // Note: The actual output may vary depending on whether 
        // the user interrupts the program or not.
        // Example output if interrupted:
        // Starting sleep for 10 seconds. Press Ctrl+C to interrupt.
        // Sleep interrupted: context canceled
    }

## Function

### SleepCause

    func SleepCause(ctx context.Context, d time.Duration) error

`SleepCause` pauses the current goroutine for the specified duration or until the context is canceled.
It returns `nil` if the sleep completes normally, or the cause of the context cancellation (as returned by `context.Cause`) if canceled.

### Sleep

    func Sleep(ctx context.Context, d time.Duration) error

`Sleep` pauses the current goroutine for the specified duration or until the context is canceled.
It returns `nil` if the sleep completes normally, or the context's error if the context is canceled before the duration elapses.

// bouncer is an input recognition package that recognizes button-presses
// of various lengths, notifies an arbitrary number of subscribers, and implements
// debouncing using the systick.
package bouncer

import (
	"errors"
	"time"

	"machine"
)

const (
	ERROR_INVALID_PRESSLENGTH = "PressLength not understood"
	ERROR_NO_OUTPUT_CHANNELS  = "New bouncer wasn't given any output channels"
)

type PressLength uint8

const (
	Bounce PressLength = iota
	ShortPress
	LongPress
	ExtraLongPress
)

type sysTickSubscriber struct {
	channel chan struct{}
}

var sysTickSubcribers []sysTickSubscriber

type Config struct {
	Short     time.Duration
	Long      time.Duration
	ExtraLong time.Duration
}

type bouncer struct {
	pin              *machine.Pin
	debounceInterval time.Duration
	shortPress       time.Duration
	longPress        time.Duration
	extraLongPress   time.Duration
	tickerCh         chan struct{}      // produced by sendTicks (relaying systick_handler ticks) -> consumed by RecognizeAndPublish (listening for ticks)
	isrChan          chan bool          // produced by the pin interrupt handler -> consumed by RecognizeAndPublish
	outChans         []chan PressLength // various channels produced by RecognizeAndPublish -> consumed by subscribers of this bouncer's events
}

type Bouncer interface {
	Configure(Config) error
	RecognizeAndPublish()
	State() bool
	Duration(PressLength) time.Duration
}

// New returns a new Bouncer (or error) with the given pin, name & channels, with default durations for
// shortPress, longPress, extraLongPress
func New(p machine.Pin, outs ...chan PressLength) (Bouncer, error) {
	if len(outs) < 1 {
		return nil, errors.New(ERROR_NO_OUTPUT_CHANNELS)
	}
	outChans := make([]chan PressLength, 0)
	for i := range outs {
		outChans = append(outChans, outs[i])
	}
	return &bouncer{
		pin:            &p,
		shortPress:     22 * time.Millisecond,
		longPress:      500 * time.Millisecond,
		extraLongPress: 1971 * time.Millisecond,
		tickerCh:       make(chan struct{}, 1),
		isrChan:        make(chan bool, 3), // Buffer interrupts during rapid bouncing
		outChans:       outChans,
	}, nil
}

// Configure sets the pin mode to InputPullup, assigns interrupt handler, overrides default durations
func (b *bouncer) Configure(cfg Config) error {
	b.pin.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	err := b.pin.SetInterrupt(machine.PinFalling|machine.PinRising, func(machine.Pin) {
		select {
		case b.isrChan <- b.pin.Get():
		default:
		}
	})
	if err != nil {
		return err
	}
	if b.shortPress > 0 {
		b.shortPress = cfg.Short
	}
	if b.longPress > 0 {
		b.longPress = cfg.Long
	}
	if b.extraLongPress > 0 {
		b.extraLongPress = cfg.ExtraLong
	}
	addSysTickConsumer(b.tickerCh)
	return nil
}

// State returns an on-demand measurement of the bouncer's pin
func (b *bouncer) State() bool {
	return b.pin.Get()
}

// RecognizeAndPublish should be a goroutine; reads pin state & sample time from channel,
// awaits completion of a buttonDown -> buttonUp sequence, recognizes press length,
// publishes the recognized press event to the button's output channel(s)
func (b *bouncer) RecognizeAndPublish() {
	ticks := 0                  // ticks will begin to increment when a button 'down' is registered
	btnDown := time.Time{}      // btnDown is the beginning time of a button press event
	dur := btnDown.Sub(btnDown) // initial duration zero
	for {
		select {
		case <-b.tickerCh:
			if ticks == 0 { // we aren't listening
				btnDown = time.Time{} // ensure this is empty because occasionally it isn't
				continue
			} else {
				ticks += 1
			}
		case up := <-b.isrChan:
			switch up {
			case true: // button is 'up'
				if ticks == 0 { // if we were awaiting a new bounce sequence to begin
					continue // ignore 'up' signal & reset the loop
				} else { // if we were awaiting the conclusion of a bounce sequence
					if ticks >= 2 { // if the interval between down & up is greater than systick interval
						dur = time.Now().Sub(btnDown) // calculate sequence duration
						ticks = 0                     // stop & reset ticks + look for new bounce sequence
						btnDown = time.Time{}         // reset button down time
						// Recognize & publish to channel(s)
						b.publish(b.recognize(dur))
					} // or ignore & await next buttonUp if debounce interval was not exceeded
				}
			case false: // button is 'down'
				if ticks == 0 { // if we were awaitng a new bounce sequence to begin
					ticks = 1            // set ticks to 1 so that ticks begins to increment with each received systick
					btnDown = time.Now() // set now as the beginning of the sequence
					continue             // reset the loop
				} // otherwise if we were awaiting the conclusion of a bounce sequence, ignore
			}
		}
	}
}

// Duration returns the duration of the passed-in PressLength
func (b *bouncer) Duration(l PressLength) time.Duration {
	switch l {
	case ShortPress:
		return b.shortPress
	case LongPress:
		return b.longPress
	case ExtraLongPress:
		return b.extraLongPress
	default:
		return 0
	}
}

// publish concurrently sends a PressLength to all channels subscribed to this Bouncer
func (b *bouncer) publish(p PressLength) {
	for i := range b.outChans {
		select {
		case b.outChans[i] <- p:
		default:
		}
	}
}

// recognize returns a PressLength resulting from a passed-in duration matching a Bouncer's durations
func (b *bouncer) recognize(d time.Duration) PressLength {
	if d >= b.extraLongPress { // duration was extraLongPress
		return ExtraLongPress
	} else if d < b.extraLongPress && d >= b.longPress { // duration was longPress
		return LongPress
	} else if d < b.longPress && d >= b.shortPress { // duration was shortPress
		return ShortPress
	}
	return Bounce // should be unreachable
}

// addSysTickConsumer appends a channel to the pkg-level SysTickSubscriber slice.
// each Bouncer is added to this slice in New and ticks are relayed by spawning RelayTicks
func addSysTickConsumer(ch chan struct{}) {
	sysTickSubcribers = append(sysTickSubcribers, sysTickSubscriber{channel: ch})
}

// sendTicks sends a signal to each Bouncer in the package-level SysTickSubscribers slice
func sendTicks() {
	if len(sysTickSubcribers) > 0 {
		for _, c := range sysTickSubcribers {
			c.channel <- struct{}{}
		}
	}
}

// Debounce relays ticks from the SysTick_Handler to all bouncers;
// and is intended to be called as a long-lived goroutine, and only once regarldess of how many bouncers you make.
// The param tickCh is intended to be the same channel spammed by your SysTick_Handler
func Debounce(tickCh chan struct{}) {
	for {
		select {
		case <-tickCh:
			sendTicks()
		}
	}
}

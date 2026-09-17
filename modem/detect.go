package modem

import (
	"context"
	"fmt"
	"log"
)

// Detect runs the hybrid detection cascade: ModemManager first (the
// richest and most common source on Linux gateways), then QMI/MBIM
// device probing, then raw AT-over-serial as a last resort. It returns
// as soon as one method finds at least one modem; a method that isn't
// installed or that finds nothing just falls through to the next one.
func Detect(ctx context.Context) ([]Modem, error) {
	modems, err := DetectModemManager(ctx)
	if err != nil {
		log.Printf("modem: ModemManager detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectQMIMBIM(ctx)
	if err != nil {
		log.Printf("modem: QMI/MBIM detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	modems, err = DetectAT(ctx)
	if err != nil {
		log.Printf("modem: AT detection unavailable: %v", err)
	} else if len(modems) > 0 {
		return modems, nil
	}

	return nil, nil
}

// GetSignal dispatches to the backend that originally detected m.
func GetSignal(ctx context.Context, m Modem) (Signal, error) {
	switch m.Method {
	case MethodModemManager:
		return getSignalModemManager(ctx, m)
	case MethodQMI:
		return getSignalQMI(ctx, m)
	case MethodMBIM:
		return getSignalMBIM(ctx, m)
	case MethodAT:
		return getSignalAT(ctx, m)
	default:
		return NaNSignal(), fmt.Errorf("unknown detection method %q", m.Method)
	}
}

package utils

import (
	"context"
	"fmt"
	"github.com/testground/sdk-go/sync"
	"math"
	"math/rand"
	"time"
)

func ValidateData(ctx context.Context, client sync.Client, runID string, validationEnabled bool,
	validationAlgorithm string, sizeIndex int) (time.Duration, error) {
	var err error
	timeToValidate := time.Duration(-1)

	if !validationEnabled {
		return timeToValidate, err
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	dataValidated := sync.State("data-validated-" + runID)
	baseValue := float64(sizeIndex) + (1.0 + rnd.Float64())
	start := time.Now()

	var sleepTime int
	switch validationAlgorithm {
	case "constant":
		sleepTime = int(10 * rnd.Float64())
	case "linear":
		sleepTime = int(baseValue)
	case "polynomial":
		sleepTime = int(baseValue * baseValue)
	case "exponential":
		sleepTime = int(math.Exp(baseValue))
	case "logarithmic":
		sleepTime = int(math.Log(baseValue))
	}

	time.Sleep(time.Duration(sleepTime) * time.Millisecond)
	timeToValidate = time.Since(start)

	_, err = client.SignalEntry(ctx, dataValidated)
	if err != nil {
		return timeToValidate, fmt.Errorf("failed to signal data validated: %w", err)
	}
	return timeToValidate, err
}

package sensors

import (
	"sync"
	"time"

	"github.com/kidoman/embd"
	"github.com/stratux/stratux/sensors/bmp5xx"
)

type BMP5XX struct {
	sensor      *bmp5xx.BMP5XX
	temperature float64
	pressure    float64
	tempErr     error
	pressErr    error
	running     bool
	mu          sync.RWMutex
	done        chan struct{}
	wg          sync.WaitGroup
	closeOnce   sync.Once
}

func NewBMP5XX(i2cbus *embd.I2CBus) (*BMP5XX, error) {
	var bmp *bmp5xx.BMP5XX

	for _, address := range []byte{bmp5xx.Address1, bmp5xx.Address2} {
		candidate := &bmp5xx.BMP5XX{Address: address, Bus: i2cbus}
		if candidate.Connected() {
			bmp = candidate
			break
		}
	}

	if bmp == nil {
		return nil, bmp5xx.ErrNotConnected
	}
	if err := bmp.Configure(); err != nil {
		return nil, err
	}

	newBmp := &BMP5XX{
		sensor:  bmp,
		running: true,
		done:    make(chan struct{}),
	}
	newBmp.wg.Add(1)
	go newBmp.run()
	return newBmp, nil
}

func (bmp *BMP5XX) run() {
	defer bmp.wg.Done()
	clock := time.NewTicker(100 * time.Millisecond)
	defer clock.Stop()

	for {
		select {
		case <-bmp.done:
			return
		case <-clock.C:
			press, pressErr := bmp.sensor.ReadPressure()
			temp, tempErr := bmp.sensor.ReadTemperature()

			bmp.mu.Lock()
			bmp.pressErr = pressErr
			if pressErr == nil {
				bmp.pressure = press
			} else {
				bmp.pressure = 0
			}
			bmp.tempErr = tempErr
			if tempErr == nil {
				bmp.temperature = temp
			} else {
				bmp.temperature = 0
			}
			bmp.mu.Unlock()
		}
	}
}

func (bmp *BMP5XX) Close() {
	bmp.closeOnce.Do(func() {
		bmp.mu.Lock()
		bmp.running = false
		bmp.mu.Unlock()
		if bmp.done != nil {
			close(bmp.done)
		}
		bmp.wg.Wait()
		if bmp.sensor != nil {
			bmp.sensor.Close()
		}
	})
}

func (bmp *BMP5XX) Temperature() (float64, error) {
	bmp.mu.RLock()
	defer bmp.mu.RUnlock()
	if !bmp.running {
		return 0, bmp5xx.ErrNotConnected
	}
	if bmp.tempErr != nil {
		return 0, bmp.tempErr
	}
	return bmp.temperature, nil
}

func (bmp *BMP5XX) Pressure() (float64, error) {
	bmp.mu.RLock()
	defer bmp.mu.RUnlock()
	if !bmp.running {
		return 0, bmp5xx.ErrNotConnected
	}
	if bmp.pressErr != nil {
		return 0, bmp.pressErr
	}
	return bmp.pressure, nil
}

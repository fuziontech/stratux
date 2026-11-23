package sensors

import (
	"sync"
	"time"

	"github.com/kidoman/embd"
	"github.com/stratux/stratux/sensors/bmp388"
)

type BMP388 struct {
	sensor      *bmp388.BMP388
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

func NewBMP388(i2cbus *embd.I2CBus) (*BMP388, error) {
	cfg := bmp388.Config{
		Temperature: bmp388.Sampling8X,
		Pressure:    bmp388.Sampling2X,
		Mode:        bmp388.Normal,
		IIR:         bmp388.Coeff0,
	}

	var bmp *bmp388.BMP388
	for _, address := range []byte{bmp388.Address, bmp388.Address2} {
		candidate := bmp388.BMP388{
			Address: address,
			Config:  cfg,
			Bus:     i2cbus,
		}
		for n := 0; n < 5; n++ {
			if candidate.Connected() {
				bmp = &candidate
				break
			}
			time.Sleep(time.Millisecond)
		}
		if bmp != nil {
			break
		}
	}

	if bmp == nil {
		return nil, bmp388.ErrNotConnected
	}
	if err := bmp.Configure(bmp.Config); err != nil {
		return nil, err
	}

	newBmp := &BMP388{
		sensor:  bmp,
		running: true,
		done:    make(chan struct{}),
	}
	newBmp.wg.Add(1)
	go newBmp.run()
	return newBmp, nil
}

func (bmp *BMP388) run() {
	defer bmp.wg.Done()
	clock := time.NewTicker(100 * time.Millisecond)
	defer clock.Stop()

	for {
		select {
		case <-bmp.done:
			return
		case <-clock.C:
			p, pErr := bmp.sensor.ReadPressure()
			t, tErr := bmp.sensor.ReadTemperature()

			bmp.mu.Lock()
			bmp.pressErr = pErr
			if pErr == nil {
				bmp.pressure = p
			} else {
				bmp.pressure = 0
			}
			bmp.tempErr = tErr
			if tErr == nil {
				bmp.temperature = t
			} else {
				bmp.temperature = 0
			}
			bmp.mu.Unlock()
		}
	}
}

func (bmp *BMP388) Close() {
	bmp.closeOnce.Do(func() {
		bmp.mu.Lock()
		bmp.running = false
		bmp.mu.Unlock()
		if bmp.done != nil {
			close(bmp.done)
		}
		bmp.wg.Wait()
		if bmp.sensor != nil {
			bmp.sensor.Config.Mode = bmp388.Sleep
			_ = bmp.sensor.Configure(bmp.sensor.Config)
		}
	})
}

// Temperature returns the current temperature in degrees C measured by the BMP388
func (bmp *BMP388) Temperature() (float64, error) {
	bmp.mu.RLock()
	defer bmp.mu.RUnlock()
	if !bmp.running {
		return 0, bmp388.ErrNotConnected
	}
	if bmp.tempErr != nil {
		return 0, bmp.tempErr
	}
	return bmp.temperature, nil
}

func (bmp *BMP388) Pressure() (float64, error) {
	bmp.mu.RLock()
	defer bmp.mu.RUnlock()
	if !bmp.running {
		return 0, bmp388.ErrNotConnected
	}
	if bmp.pressErr != nil {
		return 0, bmp.pressErr
	}
	return bmp.pressure, nil
}

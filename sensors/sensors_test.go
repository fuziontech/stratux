package sensors

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidoman/embd"
	"github.com/stratux/goflying/bmp280"
	"github.com/stratux/goflying/icm20948"
	"github.com/stratux/goflying/mpu9250"
	"github.com/stratux/stratux/sensors/bmp388"
	"github.com/stratux/stratux/sensors/bmp5xx"
	"github.com/stratux/stratux/sensors/bmx160"
)

type fakeI2CBus struct {
	embd.I2CBus
	mu      sync.Mutex
	addr    byte
	regs    map[byte][]byte
	failReg map[byte]error
	writes  [][2]byte
}

func (f *fakeI2CBus) ReadByteFromReg(addr, reg byte) (byte, error) {
	buf := make([]byte, 1)
	err := f.ReadFromReg(addr, reg, buf)
	return buf[0], err
}
func (f *fakeI2CBus) WriteByteToReg(addr, reg, value byte) error {
	return f.WriteToReg(addr, reg, []byte{value})
}

func (f *fakeI2CBus) ReadFromReg(addr, reg byte, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr != f.addr {
		return errors.New("no device at address")
	}
	if err, ok := f.failReg[reg]; ok {
		return err
	}
	if data, ok := f.regs[reg]; ok {
		copy(value, data)
		return nil
	}
	for i := range value {
		value[i] = 0
	}
	return nil
}

func (f *fakeI2CBus) WriteToReg(addr, reg byte, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr != f.addr {
		return errors.New("no device at address")
	}
	if len(value) > 0 {
		f.writes = append(f.writes, [2]byte{reg, value[0]})
	}
	return nil
}

func TestBMP388Address0x77LifecycleAndErrors(t *testing.T) {
	fake := &fakeI2CBus{
		addr: bmp388.Address2, // 0x77
		regs: map[byte][]byte{
			bmp388.RegChipId: {bmp388.ChipId},
			bmp388.RegErr:    {0x00},
			bmp388.RegCali:   make([]byte, 21),
			bmp388.RegTemp:   {0x00, 0x00, 0x80},
			bmp388.RegPress:  {0x00, 0x00, 0x80},
		},
	}
	var bus embd.I2CBus = fake
	s, err := NewBMP388(&bus)
	if err != nil {
		t.Fatalf("NewBMP388 at 0x77 failed: %v", err)
	}

	// Verify Mode was set to Normal.
	fake.mu.Lock()
	if len(fake.writes) == 0 || fake.writes[0] != [2]byte{bmp388.RegPwrCtrl, bmp388.PwrPress | bmp388.PwrTemp | byte(bmp388.Normal)} {
		t.Errorf("expected first write to enable Normal mode, got %+v", fake.writes)
	}
	fake.mu.Unlock()

	// Concurrent reads while polling.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = s.Temperature()
				_, _ = s.Pressure()
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// Inject read error and verify propagation.
	fake.mu.Lock()
	fake.failReg = map[byte]error{bmp388.RegPress: errors.New("press read fail")}
	fake.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if _, err := s.Pressure(); err == nil {
		t.Errorf("expected pressure read error to propagate")
	}

	// Double Close must be safe and put sensor into Sleep mode.
	s.Close()
	s.Close()
	if _, err := s.Pressure(); err != bmp388.ErrNotConnected {
		t.Errorf("Pressure after Close = %v, want %v", err, bmp388.ErrNotConnected)
	}
	if _, err := s.Temperature(); err != bmp388.ErrNotConnected {
		t.Errorf("Temperature after Close = %v, want %v", err, bmp388.ErrNotConnected)
	}
}

func TestBMP5XXLifecycleAndErrorPropagation(t *testing.T) {
	fake := &fakeI2CBus{
		addr: bmp5xx.Address2,
		regs: map[byte][]byte{
			bmp5xx.RegChipId:          {bmp5xx.ChipIdBMP580},
			bmp5xx.RegTemperatureData: {0x00, 0x00, 0x19}, // 25.0 C
			bmp5xx.RegPressureData:    {0x00, 0xA0, 0x62}, // ~1009.0 hPa
		},
	}
	var bus embd.I2CBus = fake
	s, err := NewBMP5XX(&bus)
	if err != nil {
		t.Fatalf("NewBMP5XX failed: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	temp, err := s.Temperature()
	if err != nil || temp != 25.0 {
		t.Errorf("Temperature = (%v, %v), want (25.0, nil)", temp, err)
	}

	// Inject read error and verify propagation (not silent 0, nil).
	fake.mu.Lock()
	fake.failReg = map[byte]error{bmp5xx.RegPressureData: errors.New("i2c err")}
	fake.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if _, err := s.Pressure(); err == nil {
		t.Errorf("expected pressure read error to propagate")
	}

	// Double Close must wait cleanly for run() and not race with Close write.
	s.Close()
	s.Close()
	if _, err := s.Temperature(); err != bmp5xx.ErrNotConnected {
		t.Errorf("Temperature after Close = %v, want %v", err, bmp5xx.ErrNotConnected)
	}
}

func TestBMP280ClosedChannelAndClose(t *testing.T) {
	ch := make(chan *bmp280.BMPData, 1)
	ch <- &bmp280.BMPData{Temperature: 21.5, Pressure: 1013.25}

	s := &BMP280{
		sensor:  &bmp280.BMP280{C: ch},
		data:    new(bmp280.BMPData),
		running: true,
		done:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run(10 * time.Millisecond)

	time.Sleep(30 * time.Millisecond)
	temp, err := s.Temperature()
	if err != nil || temp != 21.5 {
		t.Errorf("Temperature = (%v, %v), want (21.5, nil)", temp, err)
	}
	press, err := s.Pressure()
	if err != nil || press != 1013.25 {
		t.Errorf("Pressure = (%v, %v), want (1013.25, nil)", press, err)
	}

	// Closing upstream channel must cleanly stop run() without nil-data panic.
	close(ch)
	s.wg.Wait()
	if _, err := s.Temperature(); err != errBMP {
		t.Errorf("Temperature after upstream close = %v, want %v", err, errBMP)
	}
	if _, err := s.Pressure(); err != errBMP {
		t.Errorf("Pressure after upstream close = %v, want %v", err, errBMP)
	}

	// Prevent sensor.Close() from touching a nil internal channel in mock.
	s.sensor = nil
	s.Close()
	s.Close()
}

func TestIMUClosedChannelsReturnErrorNoPanic(t *testing.T) {
	// BMX160 closed channels.
	bmxC := make(chan *bmx160.BMX160Data)
	bmxCAvg := make(chan *bmx160.BMX160Data)
	close(bmxC)
	close(bmxCAvg)
	bmx := &BMX160{dev: &bmx160.BMX160{C: bmxC, CAvg: bmxCAvg}}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := bmx.Read(); gaErr == nil || magErr == nil {
		t.Errorf("BMX160.Read() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := bmx.ReadOne(); gaErr == nil || magErr == nil {
		t.Errorf("BMX160.ReadOne() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}

	// ICM20948 closed channels.
	icmC := make(chan *icm20948.MPUData)
	icmCAvg := make(chan *icm20948.MPUData)
	close(icmC)
	close(icmCAvg)
	icm := &ICM20948{mpu: &icm20948.ICM20948{C: icmC, CAvg: icmCAvg}}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := icm.Read(); gaErr == nil || magErr == nil {
		t.Errorf("ICM20948.Read() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := icm.ReadOne(); gaErr == nil || magErr == nil {
		t.Errorf("ICM20948.ReadOne() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}

	// MPU9250 closed channels.
	mpuC := make(chan *mpu9250.MPUData)
	mpuCAvg := make(chan *mpu9250.MPUData)
	close(mpuC)
	close(mpuCAvg)
	mpu := &MPU9250{mpu: &mpu9250.MPU9250{C: mpuC, CAvg: mpuCAvg}}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := mpu.Read(); gaErr == nil || magErr == nil {
		t.Errorf("MPU9250.Read() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}
	if _, _, _, _, _, _, _, _, _, _, gaErr, magErr := mpu.ReadOne(); gaErr == nil || magErr == nil {
		t.Errorf("MPU9250.ReadOne() on closed channel should return errors, got (%v, %v)", gaErr, magErr)
	}
}

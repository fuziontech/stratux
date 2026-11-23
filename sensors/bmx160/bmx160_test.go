package bmx160

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidoman/embd"
)

type mockBMXBus struct {
	embd.I2CBus
	mu           sync.Mutex
	writes       [][2]byte
	failReadFrom map[byte]error
	readErrAll   bool
	regVals      map[byte][]byte
}

func (m *mockBMXBus) WriteToReg(addr, reg byte, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(value) > 0 {
		m.writes = append(m.writes, [2]byte{reg, value[0]})
	}
	return nil
}
func (m *mockBMXBus) WriteByteToReg(addr, reg, value byte) error {
	return m.WriteToReg(addr, reg, []byte{value})
}

func (m *mockBMXBus) ReadByteFromReg(addr, reg byte) (byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErrAll {
		return 0, errors.New("bus read failed")
	}
	if err, ok := m.failReadFrom[reg]; ok {
		return 0, err
	}
	if b, ok := m.regVals[reg]; ok && len(b) > 0 {
		return b[0], nil
	}
	return 0, nil
}

func (m *mockBMXBus) ReadFromReg(addr, reg byte, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErrAll {
		return errors.New("bus read failed")
	}
	if err, ok := m.failReadFrom[reg]; ok {
		return err
	}
	if b, ok := m.regVals[reg]; ok {
		copy(value, b)
		return nil
	}
	for i := range value {
		value[i] = 0
	}
	return nil
}

func TestSetSensitivityInvalidDoesNotWrite(t *testing.T) {
	mock := &mockBMXBus{}
	dev := &BMX160{i2cbus: mock}

	if err := dev.SetGyroSensitivity(999); err == nil {
		t.Fatalf("expected error for invalid gyro sensitivity")
	}
	if err := dev.SetAccelSensitivity(99); err == nil {
		t.Fatalf("expected error for invalid accel sensitivity")
	}
	if len(mock.writes) != 0 {
		t.Errorf("expected no I2C writes on invalid sensitivities, got %+v", mock.writes)
	}
}

func TestI2CReadBytesErrorFormatting(t *testing.T) {
	mock := &mockBMXBus{
		failReadFrom: map[byte]error{0x12: errors.New("boom")},
	}
	dev := &BMX160{i2cbus: mock}
	err := dev.i2cReadBytes(0x12, make([]byte, 2))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("i2cReadBytes error = %v, want error containing 'boom'", err)
	}
}

func TestBMM150SetRegsRegisterOrder(t *testing.T) {
	mock := &mockBMXBus{}
	dev := &BMX160{i2cbus: mock}

	if err := dev.bmm150SetRegs(0x4B, 0x01); err != nil {
		t.Fatalf("bmm150SetRegs failed: %v", err)
	}
	if len(mock.writes) != 2 {
		t.Fatalf("expected 2 writes, got %d: %+v", len(mock.writes), mock.writes)
	}
	// Data must be written to AUX_IF_4 first, then register address to AUX_IF_3.
	if mock.writes[0] != [2]byte{BMI160_AUX_IF_4_ADDR, 0x01} {
		t.Errorf("first write = %+v, want AUX_IF_4=0x01", mock.writes[0])
	}
	if mock.writes[1] != [2]byte{BMI160_AUX_IF_3_ADDR, 0x4B} {
		t.Errorf("second write = %+v, want AUX_IF_3=0x4B", mock.writes[1])
	}
}

func TestReadSensorsRateErrorHandlingAndDoubleClose(t *testing.T) {
	mock := &mockBMXBus{
		regVals: map[byte][]byte{
			BMI160_GYRO_DATA_ADDR:         {0x0A, 0x00},
			BMI160_GYRO_DATA_ADDR + 0x02:  {0x00, 0x00},
			BMI160_GYRO_DATA_ADDR + 0x04:  {0x00, 0x00},
			BMI160_ACCEL_DATA_ADDR:        {0x14, 0x00},
			BMI160_ACCEL_DATA_ADDR + 0x02: {0x00, 0x00},
			BMI160_ACCEL_DATA_ADDR + 0x04: {0x00, 0x00},
			0x20:                          {0x00, 0x00},
		},
	}

	dev := &BMX160{
		i2cbus:     embd.I2CBus(mock),
		sampleRate: 200, // 5ms period; previously bugged to 50ms (20 Hz).
		scaleGyro:  1.0,
		scaleAccel: 1.0,
		cC:         make(chan *BMX160Data),
		cAvg:       make(chan *BMX160Data),
		cBuf:       make(chan *BMX160Data, bufSize),
		cClose:     make(chan struct{}),
	}
	dev.C = dev.cC
	dev.CAvg = dev.cAvg
	dev.CBuf = dev.cBuf

	dev.wg.Add(1)
	go dev.readSensors()

	// Wait 60ms — at 200 Hz (5ms), we should accumulate far more than 2 samples (at broken 20 Hz, at most 1).
	time.Sleep(60 * time.Millisecond)
	avg := <-dev.CAvg
	if avg == nil || avg.GAError != nil {
		t.Fatalf("expected valid average, got %+v", avg)
	}
	if avg.N < 5 {
		t.Errorf("expected >=5 samples in 60ms at 200 Hz, got N=%d", avg.N)
	}
	if avg.G1 != 10.0 || avg.A1 != 20.0 {
		t.Errorf("unexpected avg values: G1=%v A1=%v, want 10, 20", avg.G1, avg.A1)
	}

	// Inject read errors on one register — samples must NOT accumulate into averages.
	mock.mu.Lock()
	mock.failReadFrom = map[byte]error{BMI160_ACCEL_DATA_ADDR: errors.New("read fail")}
	mock.mu.Unlock()

	// Drain any previously buffered average, wait, then read again.
	<-dev.CAvg
	time.Sleep(30 * time.Millisecond)
	errAvg := <-dev.CAvg
	if errAvg.N != 0 || errAvg.GAError == nil {
		t.Errorf("expected N=0 and GAError!=nil on read errors, got N=%d GAError=%v", errAvg.N, errAvg.GAError)
	}

	// Double Close must not panic or block.
	dev.Close()
	dev.Close()

	// Channels should now be closed.
	if _, ok := <-dev.CAvg; ok {
		t.Errorf("expected CAvg channel to be closed after Close()")
	}
	if _, ok := <-dev.C; ok {
		t.Errorf("expected C channel to be closed after Close()")
	}
}

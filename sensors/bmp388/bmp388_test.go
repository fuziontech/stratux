package bmp388

import (
	"errors"
	"sync"
	"testing"

	"github.com/kidoman/embd"
)

type mockI2CBus struct {
	embd.I2CBus
	mu           sync.Mutex
	addr         byte
	chipID       byte
	failWriteReg map[byte]error
	statSeq      []byte
	statIdx      int
	writes       [][2]byte
	regData      map[byte][]byte
}

func (m *mockI2CBus) ReadByteFromReg(addr, reg byte) (byte, error) {
	buf := make([]byte, 1)
	err := m.ReadFromReg(addr, reg, buf)
	return buf[0], err
}
func (m *mockI2CBus) WriteByteToReg(addr, reg, value byte) error {
	return m.WriteToReg(addr, reg, []byte{value})
}

func (m *mockI2CBus) ReadFromReg(addr, reg byte, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if addr != m.addr {
		return errors.New("no ack")
	}
	switch reg {
	case RegChipId:
		value[0] = m.chipID
		return nil
	case RegErr:
		value[0] = 0
		return nil
	case RegStat:
		if len(m.statSeq) > 0 {
			idx := m.statIdx
			if idx >= len(m.statSeq) {
				idx = len(m.statSeq) - 1
			}
			value[0] = m.statSeq[idx]
			m.statIdx++
		}
		return nil
	case RegCali:
		for i := range value {
			value[i] = 0
		}
		return nil
	default:
		if data, ok := m.regData[reg]; ok {
			copy(value, data)
			return nil
		}
		return nil
	}
}

func (m *mockI2CBus) WriteToReg(addr, reg byte, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if addr != m.addr {
		return errors.New("no ack")
	}
	if err, ok := m.failWriteReg[reg]; ok {
		return err
	}
	if len(value) > 0 {
		m.writes = append(m.writes, [2]byte{reg, value[0]})
	}
	return nil
}

func TestModeConstants(t *testing.T) {
	if Forced != 0x10 {
		t.Errorf("Forced = 0x%02x, want 0x10", Forced)
	}
	if Normal != 0x30 {
		t.Errorf("Normal = 0x%02x, want 0x30", Normal)
	}
	if Sleep != 0x00 {
		t.Errorf("Sleep = 0x%02x, want 0x00", Sleep)
	}
	if Address != 0x76 || Address2 != 0x77 {
		t.Errorf("Addresses = (0x%02x, 0x%02x), want (0x76, 0x77)", Address, Address2)
	}
}

func TestConfigureChecksEveryWriteError(t *testing.T) {
	for _, reg := range []byte{RegPwrCtrl, RegOSR, RegODR, RegIIR} {
		var bus embd.I2CBus = &mockI2CBus{
			addr:         Address,
			chipID:       ChipId,
			failWriteReg: map[byte]error{reg: errors.New("i2c fail")},
		}
		d := &BMP388{Bus: &bus, Address: Address}
		err := d.Configure(Config{Mode: Normal, Temperature: Sampling8X, Pressure: Sampling2X})
		if err != errConfigWrite {
			t.Errorf("Configure failing on reg 0x%02x returned %v, want %v", reg, err, errConfigWrite)
		}
	}
}

func TestForcedModeWaitsForDRDY(t *testing.T) {
	mock := &mockI2CBus{
		addr:    Address,
		chipID:  ChipId,
		statSeq: []byte{0x00, 0x00, DRDYTemp},
		regData: map[byte][]byte{
			RegTemp: {0x01, 0x02, 0x03},
		},
	}
	var bus embd.I2CBus = mock
	d := &BMP388{Bus: &bus, Address: Address, Config: Config{Mode: Forced}}

	val, err := d.readSensorData(RegTemp)
	if err != nil {
		t.Fatalf("readSensorData failed: %v", err)
	}
	want := int64(0x03)<<16 | int64(0x02)<<8 | int64(0x01)
	if val != want {
		t.Errorf("raw temp = 0x%x, want 0x%x", val, want)
	}
	if mock.statIdx < 3 {
		t.Errorf("expected at least 3 DRDY status polls, got %d", mock.statIdx)
	}
	// Verify Forced mode write used 0x10 (PwrPress|PwrTemp|Forced = 0x13).
	if len(mock.writes) == 0 || mock.writes[0] != [2]byte{RegPwrCtrl, PwrPress | PwrTemp | byte(Forced)} {
		t.Errorf("unexpected writes: %+v", mock.writes)
	}
}

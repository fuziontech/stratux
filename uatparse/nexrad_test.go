package uatparse

import (
	"math"
	"testing"
)

func makeNexradHeader(rle, ns bool, scale, blockNum int) []byte {
	var b0 byte
	if rle {
		b0 |= 0x80
	}
	if ns {
		b0 |= 0x40
	}
	b0 |= byte((scale & 0x03) << 4)
	b0 |= byte((blockNum >> 16) & 0x0f)
	b1 := byte((blockNum >> 8) & 0xff)
	b2 := byte(blockNum & 0xff)
	return []byte{b0, b1, b2}
}

func TestDecodeNexradRLECap128(t *testing.T) {
	data := makeNexradHeader(true, false, 0, 1000)
	// 20 bytes, each runlength = 32 (31+1), intensity = 5 -> 640 bins without cap.
	for i := 0; i < 20; i++ {
		data = append(data, (31<<3)|5)
	}
	f := &UATFrame{
		Product_id: 63,
		FISB_data:  data,
	}
	f.decodeNexradFrame()
	if len(f.NEXRAD) != 1 {
		t.Fatalf("expected 1 NEXRAD block, got %d", len(f.NEXRAD))
	}
	if len(f.NEXRAD[0].Intensity) != MAX_NEXRAD_BINS {
		t.Fatalf("len(Intensity) = %d, want %d", len(f.NEXRAD[0].Intensity), MAX_NEXRAD_BINS)
	}
}

func TestDecodeNexradBlockNumValidation(t *testing.T) {
	// Invalid block_num > 607500 must be rejected.
	invalid := append(makeNexradHeader(true, false, 0, MAX_BLOCK_NUM+1), (31<<3)|2)
	fInvalid := &UATFrame{Product_id: 63, FISB_data: invalid}
	fInvalid.decodeNexradFrame()
	if len(fInvalid.NEXRAD) != 0 {
		t.Fatalf("expected block_num %d to be rejected, got %d blocks", MAX_BLOCK_NUM+1, len(fInvalid.NEXRAD))
	}

	// Valid high-latitude block_num <= 607500 must be accepted.
	valid := append(makeNexradHeader(true, false, 0, 607498), (31<<3)|2)
	fValid := &UATFrame{Product_id: 63, FISB_data: valid}
	fValid.decodeNexradFrame()
	if len(fValid.NEXRAD) != 1 {
		t.Fatalf("expected valid block_num 607498 to decode 1 block, got %d", len(fValid.NEXRAD))
	}
}

func TestDecodeNexradHighLatEmptyBlocksStrideAndWrap(t *testing.T) {
	// 60 deg N ring starts at block 405000 (ring size 450, even blocks 405000..405448).
	// Start at 405446 with L=1 and bits b+1 (0x10 -> 405448) and b+2 (0x20 -> wraps to 405000).
	data := makeNexradHeader(false, false, 0, 405446)
	data = append(data, 0x30|0x01) // L=1, b+1 and b+2 set (plus header block b+0)

	f := &UATFrame{
		Product_id: 64,
		FISB_data:  data,
	}
	f.decodeNexradFrame()

	if len(f.NEXRAD) != 3 {
		t.Fatalf("expected 3 high-lat empty blocks, got %d", len(f.NEXRAD))
	}

	wantLatN := 60.0 + BLOCK_HEIGHT
	wantLons := []float64{
		446.0*BLOCK_WIDTH - 360.0, // block 405446
		448.0*BLOCK_WIDTH - 360.0, // block 405448
		0.0,                       // block 405000 (wrapped around 0E on same ring)
	}

	for i, blk := range f.NEXRAD {
		if math.Abs(blk.LatNorth-wantLatN) > 1e-9 {
			t.Errorf("block[%d].LatNorth = %f, want %f", i, blk.LatNorth, wantLatN)
		}
		if math.Abs(blk.LonWest-wantLons[i]) > 1e-9 {
			t.Errorf("block[%d].LonWest = %f, want %f", i, blk.LonWest, wantLons[i])
		}
		if math.Abs(blk.Width-WIDE_BLOCK_WIDTH) > 1e-9 {
			t.Errorf("block[%d].Width = %f, want %f", i, blk.Width, WIDE_BLOCK_WIDTH)
		}
		if len(blk.Intensity) != 128 || blk.Intensity[0] != 1 {
			t.Errorf("block[%d] CONUS empty intensity len=%d val[0]=%d", i, len(blk.Intensity), blk.Intensity[0])
		}
	}

	// Verify mid-row high-lat block 405000 with b+1 steps by 2 to 405002 (distinct LonWest).
	midData := append(makeNexradHeader(false, false, 0, 405000), 0x10|0x01)
	fMid := &UATFrame{Product_id: 63, FISB_data: midData}
	fMid.decodeNexradFrame()
	if len(fMid.NEXRAD) != 2 {
		t.Fatalf("expected 2 blocks for 405000 + b+1, got %d", len(fMid.NEXRAD))
	}
	if math.Abs(fMid.NEXRAD[0].LonWest-0.0) > 1e-9 || math.Abs(fMid.NEXRAD[1].LonWest-2*BLOCK_WIDTH) > 1e-9 {
		t.Errorf("unexpected LonWest for 405000,405002: got %f, %f", fMid.NEXRAD[0].LonWest, fMid.NEXRAD[1].LonWest)
	}
}

func TestDecodeNexradShortAndZeroLength(t *testing.T) {
	// Short data < 4 bytes
	f1 := &UATFrame{Product_id: 63, FISB_data: []byte{0x00, 0x01, 0x02}}
	f1.decodeNexradFrame()
	if len(f1.NEXRAD) != 0 {
		t.Errorf("expected 0 blocks for short FISB_data")
	}

	// L == 0
	f2 := &UATFrame{Product_id: 63, FISB_data: append(makeNexradHeader(false, false, 0, 100), 0x00)}
	f2.decodeNexradFrame()
	if len(f2.NEXRAD) != 0 {
		t.Errorf("expected 0 blocks for L==0")
	}

	// L exceeds available bytes (L=5 requires 8 bytes, only 4 provided)
	f3 := &UATFrame{Product_id: 63, FISB_data: append(makeNexradHeader(false, false, 0, 100), 0x15)}
	f3.decodeNexradFrame()
	if len(f3.NEXRAD) != 0 {
		t.Errorf("expected 0 blocks for truncated empty-block bitmap")
	}
}

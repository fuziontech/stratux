package gpsnmea

import "testing"

func TestValidateChecksum_Valid(t *testing.T) {
	cases := []struct {
		name     string
		sentence string
		wantBody string
	}{
		{
			name:     "gpgga",
			sentence: "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47",
			wantBody: "GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,",
		},
		{
			name:     "gpgga_with_crlf",
			sentence: "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47\r\n",
			wantBody: "GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,",
		},
		{
			name:     "lowercase_hex",
			sentence: "$PFLAA,0,-10687,-22561,-10283,1,A4F2EE,136,,269,0.0,0*7e",
			wantBody: "PFLAA,0,-10687,-22561,-10283,1,A4F2EE,136,,269,0.0,0",
		},
		{
			name:     "empty_body_zero_checksum",
			sentence: "$*00",
			wantBody: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBody, ok := ValidateChecksum(tc.sentence)
			if !ok {
				t.Fatalf("ValidateChecksum(%q) returned ok=false, want true", tc.sentence)
			}
			if gotBody != tc.wantBody {
				t.Fatalf("ValidateChecksum(%q) body = %q, want %q", tc.sentence, gotBody, tc.wantBody)
			}
		})
	}
}

func TestValidateChecksum_Invalid(t *testing.T) {
	cases := []struct {
		name     string
		sentence string
	}{
		{"empty", ""},
		{"missing_dollar", "GPGGA,123519*47"},
		{"missing_asterisk", "$GPGGA,123519"},
		{"short_checksum_0", "$GPGGA,123519*"},
		{"short_checksum_1", "$GPGGA,123519*4"},
		{"non_hex_checksum", "$GPGGA,123519*ZZ"},
		{"wrong_checksum", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*48"},
		{"extra_trailing_chars", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47EXTRA"},
		{"double_asterisk", "$GPGGA,1*2*47"},
		{"sign_in_checksum", "$A*+1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ok := ValidateChecksum(tc.sentence)
			if ok || body != "" {
				t.Fatalf("ValidateChecksum(%q) = (%q, %v), want (\"\", false)", tc.sentence, body, ok)
			}
		})
	}
}

func TestAppendChecksum(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "*00"},
		{"$", "$*00"},
		{"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47"},
		{"GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,", "GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47"},
		{"$PFLAA,0,-10687,-22561,-10283,1,A4F2EE,136,,269,0.0,0", "$PFLAA,0,-10687,-22561,-10283,1,A4F2EE,136,,269,0.0,0*7E"},
	}

	for _, tc := range cases {
		got := AppendChecksum(tc.in)
		if got != tc.want {
			t.Errorf("AppendChecksum(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(tc.in) > 0 && tc.in[0] == '$' {
			body, ok := ValidateChecksum(got)
			if !ok || body != tc.in[1:] {
				t.Errorf("round-trip ValidateChecksum(%q) = (%q, %v), want (%q, true)", got, body, ok, tc.in[1:])
			}
		}
	}
}

func TestSanitizeTail(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"N12345", "N12345"},
		{"D-EFGH", "D-EFGH"},
		{"", ""},
		{"N123,45*FF\r\n$PFLAA,3,0,0,0", "N12345FFPFLAA3000"},
		{"A\x00B\x1fC\x7fD\x80E\xffF", "ABCDEF"},
		{"N12 34", "N12 34"},
	}
	for _, tc := range cases {
		if got := SanitizeTail(tc.in); got != tc.want {
			t.Errorf("SanitizeTail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClassifySatellite(t *testing.T) {
	cases := []struct {
		sv            int
		wantConst     string
		wantDisplayID int
		wantLabel     string
	}{
		{-5, ConstellationUnknown, -5, "U-5"},
		{0, ConstellationUnknown, 0, "U0"},
		{1, ConstellationGPS, 1, "G1"},
		{32, ConstellationGPS, 32, "G32"},
		{33, ConstellationSBAS, 120, "S120"},
		{51, ConstellationSBAS, 138, "S138"},
		{64, ConstellationSBAS, 151, "S151"},
		{65, ConstellationGLONASS, 1, "R1"},
		{96, ConstellationGLONASS, 32, "R32"},
		{152, ConstellationSBAS, 152, "S152"},
		{158, ConstellationSBAS, 158, "S158"},
		{193, ConstellationQZSS, 1, "Q1"},
		{202, ConstellationQZSS, 10, "Q10"},
		{301, ConstellationGalileo, 1, "E1"},
		{336, ConstellationGalileo, 36, "E36"},
		{401, ConstellationBeidou, 1, "B1"},
		{463, ConstellationBeidou, 63, "B63"},
		{464, ConstellationUnknown, 464, "U464"},
		{999, ConstellationUnknown, 999, "U999"},
	}

	for _, tc := range cases {
		gotConst, gotID := ClassifySatellite(tc.sv)
		if gotConst != tc.wantConst || gotID != tc.wantDisplayID {
			t.Errorf("ClassifySatellite(%d) = (%q, %d), want (%q, %d)",
				tc.sv, gotConst, gotID, tc.wantConst, tc.wantDisplayID)
		}
		if gotLabel := FormatSatelliteID(tc.sv); gotLabel != tc.wantLabel {
			t.Errorf("FormatSatelliteID(%d) = %q, want %q", tc.sv, gotLabel, tc.wantLabel)
		}
	}
}

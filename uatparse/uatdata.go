package uatparse

// Points can be in 3D - take care that altitude is used correctly.
type GeoPoint struct {
	Lat float64
	Lon float64
	Alt int32
}

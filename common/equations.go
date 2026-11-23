/*
	Copyright (c) 2016 AvSquirrel (https://github.com/AvSquirrel)
	Distributable under the terms of the "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	equations.go: Math and statistics library used to support AHRS
         and other fuctions of Stratux package
*/

package common

import (
	"math"
)

// LinReg calculates slope and intercept for a least squares linear regression of y[] vs x[]
// Returns invalid if fewer than two data points in each series, if series lengths are different,
// or if the regression is degenerate/non-finite.
func LinReg(x, y []float64) (slope, intercept float64, valid bool) {
	n := len(x)
	if n != len(y) || n < 2 {
		return math.NaN(), math.NaN(), false
	}

	nf := float64(n)
	var sumX, sumY float64
	for i := range x {
		sumX += x[i]
		sumY += y[i]
	}
	xBar := sumX / nf
	yBar := sumY / nf

	var sxx, sxy float64
	for i := range x {
		dx := x[i] - xBar
		dy := y[i] - yBar
		sxx += dx * dx
		sxy += dx * dy
	}

	if sxx == 0 {
		return math.NaN(), math.NaN(), false
	}

	slope = sxy / sxx
	intercept = yBar - slope*xBar
	if math.IsNaN(slope) || math.IsInf(slope, 0) || math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		return math.NaN(), math.NaN(), false
	}
	return slope, intercept, true
}

// LinRegWeighted calculates slope and intercept for a weighted least squares
// linear regression of y[] vs x[], given weights w[] for each point.
// Returns invalid if fewer than two data points in each series, if series lengths are different,
// if weights sum to zero, or if the regression is degenerate/non-finite.
func LinRegWeighted(x, y, w []float64) (slope, intercept float64, valid bool) {
	n := len(x)
	if n != len(y) || n != len(w) || n < 2 {
		return math.NaN(), math.NaN(), false
	}

	var sw, swx, swy float64
	for i := range x {
		sw += w[i]
		swx += w[i] * x[i]
		swy += w[i] * y[i]
	}

	if sw == 0 || math.IsNaN(sw) || math.IsInf(sw, 0) {
		return math.NaN(), math.NaN(), false
	}

	xBar := swx / sw
	yBar := swy / sw

	var sxx, sxy float64
	for i := range x {
		dx := x[i] - xBar
		dy := y[i] - yBar
		sxx += w[i] * dx * dx
		sxy += w[i] * dx * dy
	}

	if sxx == 0 {
		return math.NaN(), math.NaN(), false
	}

	slope = sxy / sxx
	intercept = yBar - slope*xBar
	if math.IsNaN(slope) || math.IsInf(slope, 0) || math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		return math.NaN(), math.NaN(), false
	}
	return slope, intercept, true
}

// TriCubeWeight returns the value of the tricube weight function
// at point x, for the given center and halfwidth.
func TriCubeWeight(center, halfwidth, x float64) float64 {
	if halfwidth <= 0 || math.IsNaN(halfwidth) {
		return 0
	}
	x_t := math.Abs((x - center) / halfwidth)
	if x_t < 1 {
		return math.Pow((1 - math.Pow(x_t, 3)), 3)
	}
	return 0
}

// ArrayMin calculates the minimum value in array x
func ArrayMin(x []float64) (float64, bool) {
	if len(x) < 1 {
		return math.NaN(), false
	}

	min := x[0]
	for i := range x {
		if x[i] < min {
			min = x[i]
		}
	}
	return min, true
}

// ArrayMax calculates the maximum value in array x
func ArrayMax(x []float64) (float64, bool) {
	if len(x) < 1 {
		return math.NaN(), false
	}

	max := x[0]
	for i := range x {
		if x[i] > max {
			max = x[i]
		}
	}
	return max, true
}

// ArrayRange calculates the range of values in array x
func ArrayRange(x []float64) (float64, bool) {
	max, ok1 := ArrayMax(x)
	min, ok2 := ArrayMin(x)

	if !ok1 || !ok2 {
		return math.NaN(), false
	}

	return (max - min), true
}

// Mean returns the arithmetic mean of array x
func Mean(x []float64) (float64, bool) {
	if len(x) < 1 {
		return math.NaN(), false
	}

	sum := 0.0
	nf := float64(len(x))

	for i := range x {
		sum += x[i]
	}

	return sum / nf, true
}

// Stdev estimates the sample standard deviation of array x
func Stdev(x []float64) (float64, bool) {
	if len(x) < 2 {
		return math.NaN(), false
	}

	nf := float64(len(x))
	xbar, xbarValid := Mean(x)

	if !xbarValid {
		return math.NaN(), false
	}

	sumsq := 0.0

	for i := range x {
		sumsq += (x[i] - xbar) * (x[i] - xbar)
	}

	return math.Pow(sumsq/(nf-1), 0.5), true
}

// Radians converts angle from degrees, and returns its value in radians
func Radians(angle float64) float64 {
	return angle * math.Pi / 180.0
}

// Degrees converts angle from radians, and returns its value in degrees
func Degrees(angle float64) float64 {
	return angle * 180.0 / math.Pi
}

// RadiansRel converts angle from degrees, and returns its value in radians in the range -Pi to +Pi
func RadiansRel(angle float64) float64 {
	if math.IsNaN(angle) || math.IsInf(angle, 0) {
		return math.NaN()
	}
	angle = math.Mod(angle, 360.0)
	if angle > 180.0 {
		angle -= 360.0
	} else if angle < -180.0 {
		angle += 360.0
	}
	return angle * math.Pi / 180.0
}

// DegreesRel converts angle from radians, and returns its value in the range of -180 to +180 degrees
func DegreesRel(angle float64) float64 {
	if math.IsNaN(angle) || math.IsInf(angle, 0) {
		return math.NaN()
	}
	angle = math.Mod(angle, 2*math.Pi)
	if angle > math.Pi {
		angle -= 2 * math.Pi
	} else if angle < -math.Pi {
		angle += 2 * math.Pi
	}
	return angle * 180.0 / math.Pi
}

// DegreesHdg converts angle from radians, and returns its value in the range of 0 to 360 degrees
func DegreesHdg(angle float64) float64 {
	if math.IsNaN(angle) || math.IsInf(angle, 0) {
		return math.NaN()
	}
	angle = math.Mod(angle, 2*math.Pi)
	if angle < 0 {
		angle += 2 * math.Pi
	}
	deg := angle * 180.0 / math.Pi
	if deg >= 360.0 {
		deg = 0.0
	}
	return deg
}

// RoundToInt16 rounds a float64 to an int16, returning 0 on NaN and clamping to [math.MinInt16, math.MaxInt16].
func RoundToInt16(in float64) (out int16) {
	if math.IsNaN(in) {
		return 0
	}
	rounded := math.Round(in)
	if rounded >= float64(math.MaxInt16) {
		return math.MaxInt16
	}
	if rounded <= float64(math.MinInt16) {
		return math.MinInt16
	}
	return int16(rounded)
}

/*
Distance functions based on rectangular coordinate systems
Simple calculations and "good enough" on small scale (± 1° of lat / lon)
suitable for relative distance to nearby traffic
*/

// DistRect returns distance and bearing to target #2 (e.g. traffic) from target #1 (e.g. ownship)
// Inputs are lat / lon of both points in decimal degrees
// Outputs are distance in meters and bearing in degrees (0° = north, 90° = east)
// Secondary outputs are north and east components of distance in meters (north, east positive)
func DistRect(lat1, lon1, lat2, lon2 float64) (dist, bearing, distN, distE float64) {
	radius_earth := 6371008.8 // meters; mean radius
	dLat := RadiansRel(lat2 - lat1)
	avgLat := RadiansRel((lat2 + lat1) / 2)
	dLon := RadiansRel(lon2 - lon1)
	distN = dLat * radius_earth
	distE = dLon * radius_earth * math.Abs(math.Cos(avgLat))
	dist = math.Pow(distN*distN+distE*distE, 0.5)
	bearing = math.Atan2(distE, distN)
	bearing = DegreesHdg(bearing)
	return
}

/*
Distance functions: Polar coordinate systems
More accurate over longer distances
*/

// Distance calculates great-circle distance between two points using the haversine formula.
// Inputs are lat / lon of both points in decimal degrees.
// Outputs are distance in meters and initial bearing to the target from origin in degrees (0° = north, 90° = east).
func Distance(lat1, lon1, lat2, lon2 float64) (dist, bearing float64) {
	const radiusEarth = 6371008.8 // meters; mean radius

	lat1 = Radians(lat1)
	lon1 = Radians(lon1)
	lat2 = Radians(lat2)
	lon2 = Radians(lon2)

	dLat := lat2 - lat1
	dLon := lon2 - lon1
	sinHalfLat := math.Sin(dLat / 2)
	sinHalfLon := math.Sin(dLon / 2)
	a := sinHalfLat*sinHalfLat + math.Cos(lat1)*math.Cos(lat2)*sinHalfLon*sinHalfLon
	if a < 0 {
		a = 0
	} else if a > 1 {
		a = 1
	}
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	dist = radiusEarth * c

	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon)
	y := math.Sin(dLon) * math.Cos(lat2)

	bearing = DegreesHdg(math.Atan2(y, x))

	return
}

// CalcAltitude determines the pressure altitude (feet) from the atmospheric pressure (hPa).
// Returns 0 if press is non-positive, NaN, or infinite.
func CalcAltitude(press float64, altoffset int) (altitude float64) {
	if press <= 0 || math.IsNaN(press) || math.IsInf(press, 0) {
		return 0
	}
	altitude = 145366.45*(1.0-math.Pow(press/1013.25, 0.190284)) + float64(altoffset)
	return
}

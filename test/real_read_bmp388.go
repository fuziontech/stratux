package main

import (
	"fmt"
	"log"
	"math"
	"time"

	"github.com/kidoman/embd"
	_ "github.com/kidoman/embd/host/all"
	"github.com/stratux/stratux/sensors/bmp388"
)

func main() {
	i2cbus := embd.NewI2CBus(1)
	bmp := bmp388.BMP388{Bus: &i2cbus, Address: bmp388.Address}
	if err := bmp.Configure(bmp.Config); err != nil {
		log.Fatalf("Failed to configure BMP388: %v", err)
	}

	fmt.Println("t,temp,press,alt")

	start := time.Now()
	clock := time.NewTicker(time.Millisecond)
	defer clock.Stop()

	for range clock.C {
		press, err := bmp.ReadPressure()
		if err != nil {
			log.Printf("ReadPressure error: %v", err)
			continue
		}
		temp, err := bmp.ReadTemperature()
		if err != nil {
			log.Printf("ReadTemperature error: %v", err)
			continue
		}
		alt := 44330.0 * (1.0 - math.Pow(press/1013.25, 1.0/5.255))
		fmt.Printf("%.3f,%.2f,%.2f,%.2f\n", time.Since(start).Seconds(), temp, press, alt)
	}
}

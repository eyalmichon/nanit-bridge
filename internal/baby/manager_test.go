package baby

import (
	"testing"
	"time"

	pb "nanit-bridge/internal/nanit/nanitpb"
)

func int32Ptr(v int32) *int32 { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestApplySensorData(t *testing.T) {
	tests := []struct {
		name   string
		in     *pb.SensorData
		assert func(t *testing.T, s SensorState)
	}{
		{
			name: "temperature from milli",
			in: &pb.SensorData{
				SensorType: pb.SensorType_TEMPERATURE.Enum(),
				ValueMilli: int32Ptr(23500),
			},
			assert: func(t *testing.T, s SensorState) {
				if s.Temperature != 23.5 {
					t.Fatalf("Temperature = %v, want 23.5", s.Temperature)
				}
			},
		},
		{
			name: "humidity from value",
			in: &pb.SensorData{
				SensorType: pb.SensorType_HUMIDITY.Enum(),
				Value:      int32Ptr(44),
			},
			assert: func(t *testing.T, s SensorState) {
				if s.Humidity != 44 {
					t.Fatalf("Humidity = %v, want 44", s.Humidity)
				}
			},
		},
		{
			name: "light from milli",
			in: &pb.SensorData{
				SensorType: pb.SensorType_LIGHT.Enum(),
				ValueMilli: int32Ptr(9000),
			},
			assert: func(t *testing.T, s SensorState) {
				if s.Light != 9 {
					t.Fatalf("Light = %v, want 9", s.Light)
				}
			},
		},
		{
			name: "night true",
			in: &pb.SensorData{
				SensorType: pb.SensorType_NIGHT.Enum(),
				Value:      int32Ptr(1),
			},
			assert: func(t *testing.T, s SensorState) {
				if !s.IsNight {
					t.Fatalf("IsNight = false, want true")
				}
			},
		},
		{
			name: "cry alert",
			in: &pb.SensorData{
				SensorType: pb.SensorType_CRY.Enum(),
				IsAlert:    boolPtr(true),
			},
			assert: func(t *testing.T, s SensorState) {
				if !s.CryDetected {
					t.Fatalf("CryDetected = false, want true")
				}
				if s.CryDetectedAt.IsZero() {
					t.Fatalf("CryDetectedAt should be set")
				}
			},
		},
		{
			name: "cry not alert",
			in: &pb.SensorData{
				SensorType: pb.SensorType_CRY.Enum(),
				IsAlert:    boolPtr(false),
			},
			assert: func(t *testing.T, s SensorState) {
				if s.CryDetected {
					t.Fatalf("CryDetected = true, want false")
				}
				if !s.CryDetectedAt.IsZero() {
					t.Fatalf("CryDetectedAt should be zero")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s SensorState
			before := time.Now()
			applySensorData(&s, tc.in)
			tc.assert(t, s)
			if tc.name == "cry alert" && s.CryDetectedAt.Before(before) {
				t.Fatalf("CryDetectedAt %v before %v", s.CryDetectedAt, before)
			}
		})
	}
}

func TestApplySensorDataNil(t *testing.T) {
	s := SensorState{Temperature: 10}
	applySensorData(&s, nil)
	if s.Temperature != 10 {
		t.Fatalf("state mutated unexpectedly: %+v", s)
	}
}

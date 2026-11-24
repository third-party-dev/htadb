// Command fakeadb runs a fake adb server with fake devices, for end-to-end
// tests and UI work without hardware. With -h264, the first device's
// mirroring server streams that Annex B clip (looped).
//
//	fakeadb -addr 127.0.0.1:5037 -h264 clip.h264 -devices R58M12345,emulator-5554
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"htadb/internal/adb/adbtest"
	"htadb/internal/mirror/mirrortest"
	"htadb/internal/mirror/servers"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5037", "listen address")
	clip := flag.String("h264", "", "H.264 Annex B clip to stream when mirroring")
	devs := flag.String("devices", "R58M12345,emulator-5554", "comma-separated serials")
	width := flag.Uint("width", 0, "video width reported (default: from SPS or 1080)")
	height := flag.Uint("height", 0, "video height reported")
	fail := flag.String("fail-versions", "", "comma-separated server versions that fail at launch (fallback testing)")
	failOn := flag.String("fail-on", "", "comma-separated serials -fail-versions applies to (default: all)")
	verbose := flag.Bool("v", false, "log requests")
	flag.Parse()

	srv, err := adbtest.NewServer(*addr)
	if err != nil {
		log.Fatal(err)
	}
	if *verbose {
		srv.Log = log.Printf
	}
	newFake := func(serial string) *mirrortest.Server {
		f := &mirrortest.Server{FailVersions: map[string]string{}, Loop: true, FPS: 30, Width: uint32(*width), Height: uint32(*height),
			DeviceClipboard: "hello from the fake device"}
		if *failOn == "" || strings.Contains(","+*failOn+",", ","+serial+",") {
			for _, v := range strings.Split(*fail, ",") {
				if v != "" {
					f.FailVersions[v] = "Could not create video encoder (simulated failure of " + v + ")"
				}
			}
		}
		return f
	}
	var cfg []byte
	var frames []mirrortest.Frame
	if *clip != "" {
		b, err := os.ReadFile(*clip)
		if err != nil {
			log.Fatal(err)
		}
		cfg, frames = mirrortest.SplitAnnexB(b)
		if len(cfg) == 0 || len(frames) == 0 {
			log.Fatalf("%s: no SPS/PPS or frames found", *clip)
		}
		log.Printf("fakeadb: %s: %d frames", *clip, len(frames))
	}
	for i, s := range strings.Split(*devs, ",") {
		d := adbtest.NewDevice(s)
		if strings.HasPrefix(s, "emulator-") {
			d.Model, d.Product, d.DeviceName = "sdk_gphone64_x86_64", "sdk_gphone64_x86_64", "emu64xa"
		} else {
			d.Model, d.Product, d.DeviceName = "SM_G973F", "beyond1lteeea", "beyond1"
		}
		d.WriteFile("/sdcard/Download/notes.txt", []byte("fake file\n"), 0o644)
		d.WriteFile("/sdcard/DCIM/Camera/IMG_0001.jpg", []byte("JPEG"), 0o644)
		d.Props["ro.product.model"] = strings.ReplaceAll(d.Model, "_", " ")
		fake := newFake(s)
		if cfg != nil {
			fake.Config, fake.Frames = cfg, frames
		}
		fake.Install(d)
		// A jar left behind by an earlier session (verified against pins
		// only for real jars; fake content here).
		if i == 0 {
			d.WriteFile(servers.DeviceDir+"/scrcpy-server-v4.0.jar", mirrortest.FakeJar("4.0"), 0o644)
		}
		srv.AddDevice(d)
	}
	fmt.Printf("FAKEADB_LISTENING %s\n", srv.Addr())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}

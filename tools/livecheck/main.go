// Command livecheck prints what the baseline poll currently knows about
// associated clients, straight from the devices. Read-only.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"

	_ "modernc.org/sqlite"
)

func main() {
	ctx := context.Background()
	db, _ := store.Open(ctx, "sqlite", os.Args[1])
	defer db.Close()
	devs, _ := db.Devices(ctx)
	clients, _ := db.Clients(ctx, 0, 500)
	byMAC := map[string]string{}
	for _, c := range clients {
		byMAC[c.MAC] = c.Name
	}
	for _, d := range devs {
		c := ubus.New(ubus.Options{Host: d.Host, Timeout: 10 * time.Second})
		if c.Login(ctx, "root", "") != nil {
			continue
		}
		var ifs struct {
			Devices []string `json:"devices"`
		}
		_ = c.Call(ctx, "iwinfo", "devices", nil, &ifs)
		fmt.Printf("=== %s ===\n", d.Name)
		for _, iface := range ifs.Devices {
			var out struct {
				Clients map[string]struct {
					Signal *int `json:"signal"`
				} `json:"clients"`
			}
			if err := c.Call(ctx, "hostapd."+iface, "get_clients", nil, &out); err != nil {
				continue
			}
			for mac, st := range out.Clients {
				name := byMAC[mac]
				if name == "" {
					name = "(not in the clients table)"
				}
				sig := "no RSSI reported"
				if st.Signal != nil {
					sig = fmt.Sprintf("%d dBm", *st.Signal)
				}
				fmt.Printf("  %s  %-22s %-12s on %s\n", mac, name, sig, iface)
			}
		}
		c.Close()
	}
}

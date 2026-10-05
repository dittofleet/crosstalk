package cmd

import (
	"errors"
	"fmt"

	"github.com/dittofleet/crosstalk/internal/config"
	"github.com/dittofleet/crosstalk/internal/secret"
)

// Key makes a new key and prints it with the hub token derived from it.
// `key token` prints the token for the key this machine already has, for
// setting a hub up again.
func Key(args []string) error {
	if len(args) == 1 && args[0] == "token" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		keys, err := secret.Derive(cfg.Key)
		if err != nil {
			return err
		}
		fmt.Println(keys.HubToken)
		return nil
	}
	if len(args) != 0 {
		return errors.New("usage: crosstalk key [token]")
	}

	key, err := secret.NewKey()
	if err != nil {
		return err
	}
	keys, err := secret.Derive(key)
	if err != nil {
		return err
	}
	fmt.Printf(`Key, to enter on each of your machines with crosstalk join. Keep it
secret: any machine that has it is one of your devices.

  %s

Hub token, to set on the hub with wrangler secret put HUB_TOKEN. It lets a
device connect and cannot be turned back into the key.

  %s
`, key, keys.HubToken)
	return nil
}

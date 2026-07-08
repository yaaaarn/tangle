# ⚡ tangle

a battery monitor daemon. listens to upower events over dbus and fires configurable commands.

## table of contents

- [install](#install)
  - [go](#go)
  - [nix flake](#nix-flake)
- [usage](#usage)
- [configuration](#configuration)
- [nixos module](#nixos-module)
- [dev](#dev)
- [license](#license)

## install

### go

```
go install github.com/yaaaarn/tangle@latest
```

### nix flake

add to your `flake.nix` inputs:

```nix
tangle = {
  url = "github:yaaaarn/tangle";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

then add `tangle.packages.${system}.default` to your `environment.systemPackages` or home-manager packages.

## usage

```
tangle
```

looks for a `config.yaml` in the current directory, `~/.config/tangle/config.yaml`, `/etc/tangle/config.yaml`, or `/var/lib/tangle/config.yaml`.

## configuration

```yaml
actions:
  - trigger_on_state: "Discharging"
    threshold_percentage: 20
    operator: "<="
    command:
      - notify-send
      - "--urgency=normal"
      - "--icon=battery-low"
      - "battery low ({percent}%)"
```

| field | description |
|---|---|
| `trigger_on_state` | `Charging`, `Discharging`, `Full` |
| `threshold_percentage` | percentage to compare against |
| `operator` | `any`, `==`, `<=`, `>=` |
| `command` | command + args to execute (`{percent}` and `{state}` are substituted) |

## nixos module

```nix
{
  imports = [ tangle.nixosModules.default ];

  services.tangle = {
    enable = true;
    actions = [
      {
        trigger_on_state = "Discharging";
        threshold_percentage = 20;
        operator = "<=";
        command = [
          "notify-send"
          "--urgency=normal"
          "--icon=battery-low"
          "battery low ({percent}%)"
        ];
      }
    ];
  };
}
```

> a home-manager module is also available: `tangle.hmModules.default`.

## dev

```bash
# enter the dev shell (if using nix)
nix develop

# build
go build .
```

## license

mit

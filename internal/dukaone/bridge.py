#!/usr/bin/env python3
import argparse
import json
import sys
import time

from dukaonesdk.dukaclient import DukaClient
from dukaonesdk.mode import Mode
from dukaonesdk.speed import Speed


MODE_MAP = {
    "out": Mode.ONEWAY,
    "oneway": Mode.ONEWAY,
    "inout": Mode.TWOWAY,
    "twoway": Mode.TWOWAY,
    "in": Mode.IN,
}

SPEED_MAP = {
    "off": Speed.OFF,
    "low": Speed.LOW,
    "medium": Speed.MEDIUM,
    "high": Speed.HIGH,
    "manual": Speed.MANUAL,
}


def refresh_device(client, device, timeout=4.0):
    updater = getattr(client, "_DukaClient__update_device_status", None)
    if updater is not None:
        updater(device)
    end = time.time() + timeout
    while time.time() < end:
        if device.mode is not None or device.speed is not None:
            return
        time.sleep(0.1)


def serialize(device, configured_host):
    speed = None
    if device.speed is not None:
        try:
            speed = device.speed.name.lower()
        except AttributeError:
            speed = str(device.speed).lower()

    mode = None
    if device.mode == Mode.ONEWAY:
        mode = "out"
    elif device.mode == Mode.TWOWAY:
        mode = "inout"
    elif device.mode == Mode.IN:
        mode = "in"

    is_on = None
    if device.speed is not None:
        is_on = device.speed != Speed.OFF

    return {
        "device_id": device.device_id,
        "ip_address": device.ip_address,
        "configured_host": configured_host,
        "speed": speed,
        "mode": mode,
        "manual_speed": device.manualspeed,
        "is_on": is_on,
        "humidity": device.humidity,
        "filter_alarm": bool(device.filter_alarm),
        "filter_timer": device.filter_timer,
        "fan_rpm": device.fan1rpm,
        "firmware_version": device.firmware_version,
        "firmware_date": device.firmware_date,
        "unit_type": device.unit_type,
    }


def apply_attrs(client, device, attrs):
    mode = attrs.get("mode")
    if mode is None:
        mode = attrs.get("ventilationMode")
    if mode is not None:
        normalized = str(mode).strip().lower()
        if normalized not in MODE_MAP:
            raise ValueError(f"unsupported DukaOne mode {mode!r}")
        client.set_mode(device, MODE_MAP[normalized])
        time.sleep(0.2)

    preset = attrs.get("preset")
    if preset is None:
        preset = attrs.get("speed")
    if preset is not None:
        normalized = str(preset).strip().lower()
        if normalized not in SPEED_MAP:
            raise ValueError(f"unsupported DukaOne preset {preset!r}")
        client.set_speed(device, SPEED_MAP[normalized])
        time.sleep(0.2)

    manual_speed = attrs.get("manualSpeed")
    if manual_speed is not None:
        manual_speed = int(manual_speed)
        if manual_speed < 0 or manual_speed > 255:
            raise ValueError("manualSpeed must be between 0 and 255")
        client.set_manual_speed(device, manual_speed)
        time.sleep(0.2)

    reset_filter = attrs.get("resetFilter")
    if reset_filter is None:
        reset_filter = attrs.get("resetFilterAlarm")
    if reset_filter:
        client.reset_filter_alarm(device)
        time.sleep(0.2)

    if "isOn" in attrs:
        is_on = bool(attrs["isOn"])
        if is_on:
            client.turn_on(device)
        else:
            client.turn_off(device)
        time.sleep(0.2)


def run(args):
    client = DukaClient()
    try:
        if args.command == "discover":
            found_ids = set()

            def on_found(device_id):
                found_ids.add(device_id.strip())

            client.search_devices(on_found)
            time.sleep(args.timeout)
            devices = []
            for device_id in sorted(found_ids):
                device = client.validate_device(device_id, args.password, args.ip_address)
                if device is None:
                    continue
                try:
                    device.wait_for_initialize()
                except Exception:
                    pass
                refresh_device(client, device)
                devices.append(serialize(device, args.ip_address))
            print(json.dumps({"devices": devices}))
            return

        device = client.add_device(args.device_id, args.password, args.ip_address)
        device.wait_for_initialize()
        refresh_device(client, device)

        if args.command == "set":
            attrs = json.loads(args.attrs_json)
            apply_attrs(client, device, attrs)
            refresh_device(client, device)

        print(json.dumps(serialize(device, args.ip_address)))
    finally:
        client.close()


def main():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    discover = subparsers.add_parser("discover")
    discover.add_argument("--password", default="1111")
    discover.add_argument("--ip-address", default="<broadcast>")
    discover.add_argument("--timeout", type=float, default=2.0)

    for command in ("state", "set"):
        current = subparsers.add_parser(command)
        current.add_argument("--device-id", required=True)
        current.add_argument("--password", default="1111")
        current.add_argument("--ip-address", default="<broadcast>")
        if command == "set":
            current.add_argument("--attrs-json", required=True)

    args = parser.parse_args()
    try:
        run(args)
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()

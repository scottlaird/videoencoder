#!/bin/sh
#
# turbostat-setup.sh: lets you run turbostat without sudo on Ubuntu, so
# encbench can record each encode's CPU clock, IPC, power and
# temperature. Run it as yourself; it uses sudo for each step.
#
# Re-run it after a kernel upgrade: Ubuntu's /usr/bin/turbostat is a
# script that runs a copy of turbostat for each kernel, and only this
# kernel's copy gets the capabilities.
#
# What it allows: anyone in the msr group can read (not write) the
# CPUs' model-specific registers, and run turbostat with the
# capabilities its man page asks for. turbostat doesn't pass them on
# to the command it runs, so encbench's ffmpeg runs unprivileged.
set -eux

# 1. Make the msr devices, /dev/cpu/*/msr, readable by group msr, and
#    add yourself to it. They're root-only by default.
sudo groupadd -f msr
echo 'SUBSYSTEM=="msr", KERNEL=="msr[0-9]*", GROUP="msr", MODE="0640"' | sudo tee /etc/udev/rules.d/90-msr.rules
echo msr | sudo tee /etc/modules-load.d/msr.conf
sudo modprobe msr
sudo udevadm control --reload
sudo udevadm trigger --subsystem-match=msr
sudo usermod -aG msr "$USER"

# 2. Give this kernel's turbostat the capabilities it needs (reading
#    the msr devices, perf counters, and moving itself between CPUs),
#    and let only the msr group run it.
turbostat=/usr/lib/linux-tools/$(uname -r)/turbostat
sudo chgrp msr "$turbostat"
sudo chmod 0750 "$turbostat"
sudo setcap cap_sys_admin,cap_sys_rawio,cap_sys_nice=+ep "$turbostat"

# 3. Log out and back in, so the msr group applies, then check that
#    this prints a line of numbers without sudo:
#
#      turbostat --quiet --Summary true

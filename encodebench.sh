#!/bin/bash

FLAGS="$*"

echo "====> encoding with $FLAGS"
time /usr/local/bin/ffmpeg -hide_banner -loglevel warning -nostats -y -i 20250301-deception1-1080p.mov $FLAGS out.mp4


echo " ---> size"
ls -l out.mp4

echo " ---> calculating VMAF (~3 minutes)"
/usr/local/bin/ffmpeg -loglevel info -stats -i out.mp4 -i 20250301-deception1-1080p.mov -i /space/photography/Videos/Backgrounds/20250301-deception1b.mov -lavfi libvmaf -f null - |& egrep 'VMAF score'

#!/bin/bash

# Usage:
#
# $ START=18 STOP=25 PREFIX=foo FILE=in.mov ./parallelencodebench.sh -c:v libx265 -crf
#
# This will calculate the timings of ffmpeg with the specified
# options, with `-crf` values from 18 to 25, and then calculate VMAF
# values for all runs in parallel once the encodes are done.
#
# Output results will be in $PREFIX-$quality.*
#
# Defaults to use /usr/local/bin/ffmpeg rather than the one in the
# path for uninteresting reasons.  Use FFMPEG=ffmpeg to fix that.
#

FLAGS="$@"
PRE="${PREFIX:-out}"
INFILE="${FILE:-20250301-deception1-1080p.mov}"
FFMPEG="${FFMPEG:-/usr/local/bin/ffmpeg}"
INC="${INC:-1}"
LOGFILE="${LOGFILE:-encodebench.log}"

for quality in `seq $START $INC $STOP`; do
    echo "====> encoding with $FLAGS $quality"
    /usr/bin/time -p -o $PRE-$quality.times.txt $FFMPEG -hide_banner -loglevel warning -nostats -y -i $INFILE $FLAGS $quality $PRE-$quality.mp4 >& $PRE-$quality.encoding.txt > /dev/null

    ls -l $PRE-$quality.mp4
done

for quality in `seq $START $INC $STOP`; do 
    echo " ---> calculating VMAF for $quality"
    ($FFMPEG -loglevel info -stats -i $PRE-$quality.mp4 -i $INFILE -lavfi libvmaf=n_threads=4 -f null - |& egrep 'VMAF score') | sed -E 's/.*(VMAF score: [0-9.]+).*/\1/' > $PRE-$quality.vmaf.txt &
    ($FFMPEG -loglevel info -stats -i $PRE-$quality.mp4 -i $INFILE -lavfi libvmaf='n_threads=4:model=version=vmaf_v0.6.1neg' -f null - |& egrep 'VMAF score') | sed -E 's/.*(VMAF score: [0-9.]+).*/\1/' > $PRE-$quality.vmafneg.txt &
done

echo "====> waiting for VMAF values"
wait $(jobs -rp)

echo "====> results"
echo "quality,file_kb,real_seconds,user_seconds,vmaf,vmafneg"
for quality in `seq $START $INC $STOP`; do
    size=`ls -s $PRE-$quality.mp4 | cut -d' ' -f1`
    real=`grep real $PRE-$quality.times.txt | cut -d' ' -f2`
    user=`grep user $PRE-$quality.times.txt | cut -d' ' -f2`
    vmaf=`cut -d: -f2 $PRE-$quality.vmaf.txt`
    vmafneg=`cut -d: -f2 $PRE-$quality.vmafneg.txt`
    echo $quality,$size,$real,$user,$vmaf,$vmafneg
    echo $quality,$size,$real,$user,$vmaf,$vmafneg,"\"$FLAGS $quality\"" >> $LOGFILE
done

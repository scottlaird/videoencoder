#!/bin/bash

for a in `seq 3 12`; do 
    START=32 STOP=40 INC=1 ./parallelencodebench.sh -c:v libsvtav1 -preset $a -crf
done

exit
START=22 STOP=35 INC=1 ./parallelencodebench.sh -c:v libsvtav1 -preset 2 -rc 2 -aq-mode 2 -qp

# exit
# Default
START=31 STOP=35 INC=1 ./parallelencodebench.sh -c:v libsvtav1 -crf
START=32 STOP=38 INC=1 ./parallelencodebench.sh -c:v libsvtav1 -preset 2 -crf
# Like -crf, but forcing VBR
START=22 STOP=30 INC=1 ./parallelencodebench.sh -c:v libsvtav1 -rc 2 -aq-mode 2 -qp


exit

START=32.1 STOP=32.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v
START=32.1 STOP=32.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -g 600 -keyint_min 600 -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v

exit

START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -c:v libx265 -preset slow -tune fastdecode -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -g 600 -keyint_min 600 -c:v libx265 -preset slow -tune fastdecode -crf

exit

START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf 
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=19.1 STOP=19.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -g 600 -keyint_min 600 -c:v libx265 -preset slow -tune fastdecode -crf
START=19.1 STOP=19.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=32.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p2 -tune uhq -cq:v
START=19.1 STOP=19.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p -g 600 -keyint_min 600 -c:v libx265 -preset slow -tune fastdecode -crf
START=19.1 STOP=19.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -c:v libx265 -tune fastdecode -crf
START=16.1 STOP=16.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p -c:v libx265 -crf
START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p -c:v libx265 -crf


exit
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv422p -c:v libx265 -tune fastdecode -crf

START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p -c:v libx265 -tune fastdecode -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -tune fastdecode -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -tune fastdecode -crf

# Refine INC=0.1
START=34.1 STOP=34.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p1 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p3 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p4 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p5 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p6 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v



exit

START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv422p -c:v libx265 -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p -c:v libx265 -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv422p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv422p -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf


## Basic libx265
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -crf
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh  -pix_fmt yuv422p10le -c:v libx265 -crf
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh  -pix_fmt yuv420p10le -c:v libx265 -crf

START=13 STOP=17 INC=0.5 ./parallelencodebench.sh  -c:v libx265 -preset ultrafast -crf
START=16.1 STOP=16.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset superfast -crf
START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset veryfast -crf
START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset faster -crf
START=17.1 STOP=17.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset fast -crf
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset medium -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset slow -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset slower -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -preset veryslow -crf

# Should be defaults
#START=16 STOP=24 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -profile:v main10 -crf  # error

# Encoding options
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh  -c:v libx265 -tune fastdecode -crf

# Frame rate?
#START=16 STOP=24 INC=0.1 ./parallelencodebench.sh  -r 30 -c:v libx265 -crf

# GOP
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf


## hevc_nvenc
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh  -c:v hevc_nvenc -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -cq:v
START=27.1 STOP=27.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p1 -cq:v
START=27.1 STOP=27.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p2 -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p3 -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p4 -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p5 -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p6 -cq:v
START=26.1 STOP=26.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p7 -cq:v

START=34.1 STOP=35 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p1 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p2 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p3 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p4 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p5 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p6 -tune uhq -cq:v
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v

#START=22 STOP=34 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv422p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v  # error
START=33.1 STOP=33.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -g 600 -keyint_min 600 -cq:v

START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p1 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p2 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p3 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p4 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p5 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p6 -tune uhq -cq:v
START=22 STOP=35 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v





## Done

exit

## Basic libx265
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -pix_fmt yuv422p10le -c:v libx265 -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -pix_fmt yuv420p10le -c:v libx265 -crf

START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset ultrafast -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset superfast -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset veryfast -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset faster -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset fast -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset medium -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset slow -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset slower -crf
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -preset veryslow -crf

# Should be defaults
#START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -profile:v main10 -crf  # error

# Encoding options
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -c:v libx265 -tune fastdecode -crf

# Frame rate?
START=16 STOP=24 INC=1 ./parallelencodebench.sh  -r 30 -c:v libx265 -crf

# GOP
START=16 STOP=24 INC=1 ./parallelencodebench.sh -g 600 -keyint_min 600 -c:v libx265 -preset slow -crf


## hevc_nvenc
START=22 STOP=34 INC=1 ./parallelencodebench.sh  -c:v hevc_nvenc -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p1 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p2 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p3 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p4 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p5 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p6 -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p7 -cq:v

START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p1 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p2 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p3 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p4 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p5 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p6 -tune uhq -cq:v
START=22 STOP=34 INC=1 ./parallelencodebench.sh -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v

#START=22 STOP=34 INC=1 ./parallelencodebench.sh -pix_fmt yuv422p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -cq:v  # error
START=22 STOP=34 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -rc vbr -c:v hevc_nvenc -preset p7 -tune uhq -g 600 -keyint_min 600 -cq:v





#### old
exit

START=16 STOP=23 INC=1 ./parallelencodebench.sh -c:v libx265 -crf
START=16 STOP=23 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -crf
START=16 STOP=23 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset slow -crf
START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset slow -crf

START=16 STOP=23 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset medium -crf
START=18.1 STOP=18.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset medium -crf
START=16 STOP=23 INC=1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset medium -profile:v main10 -crf

START=20.1 STOP=20.9 INC=0.1 ./parallelencodebench.sh -pix_fmt yuv420p10le -c:v libx265 -preset slower -crf



## Next

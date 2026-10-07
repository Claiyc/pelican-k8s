#!/bin/sh
# The "game" of the live placement suite. Every start appends a line to
# /home/container/starts and writes its PID to /home/container/pid; the stop
# command appends a line to /home/container/stops. With /home/container/crash
# present it exits with code 3 right after starting.
d=/home/container
echo "$$" >> "$d/starts"
echo "$$" > "$d/pid"
if [ -f "$d/crash" ]; then
  echo "crashing on request"
  exit 3
fi
echo "Server ready"
while true; do
  if read -r -t 1 line; then
    if [ "$line" = stop ]; then
      echo "$$" >> "$d/stops"
      echo "stopping"
      exit 0
    fi
  elif [ $? -le 128 ]; then
    # stdin closed: keep running until signalled.
    sleep 1
  fi
done

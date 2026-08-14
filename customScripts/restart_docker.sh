#!/bin/bash

# Restarts the docker compose file instead of having to type it all the time
cd /opt/viiwork;
sudo docker compose down;
sudo docker compose up -d;

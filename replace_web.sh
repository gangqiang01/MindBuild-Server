#!/bin/bash
set -e

mkdir -p ./frontend
rm -rf ./frontend/*

scp -r sgq@172.21.73.80:/home/sgq/PROJECT/FRONTEND/Weknora/dist/* ./frontend/

ls -l ./frontend

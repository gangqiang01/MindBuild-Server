#!/bin/bash
set -e

mkdir -p ./frontend
rm -rf ./frontend/*

cp -r /home/sgq/MYPROJECT/FRONTEND/mindBuilder/dist/* ./frontend/

ls -l ./frontend

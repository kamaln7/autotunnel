#!/usr/bin/env bash
mkdir build
cp appicon.png build/
wails build -production -package

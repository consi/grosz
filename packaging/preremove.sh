#!/bin/sh
set -e

systemctl stop grosz.service || true
systemctl disable grosz.service || true

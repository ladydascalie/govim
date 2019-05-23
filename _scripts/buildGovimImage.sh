#!/usr/bin/env bash

set -euo pipefail

source "${BASH_SOURCE%/*}/gen_maxVersions_genconfig.bash"

cd "${BASH_SOURCE%/*}"

# Usage; either:
#
#   buildGovimImage.sh
#   buildGovimImage.sh VIM_FLAVOR VIM_VERSION GO_VERSION
#
# Note that VIM_FLAVOR can be one of vim, gvim or neovim and
# VIM_VERSION is a version pertaining to any of them.

if [ "$#" -eq 3 ]
then
	VIM_FLAVOR="$1"
	VIM_VERSION="$2"
	GO_VERSION="$3"
else
	# If not provided we default to testing against vim. MAX_VIM_VERSION pertains
	# to a version of Vim, not neovim.
	VIM_FLAVOR="${VIM_FLAVOR:-vim}"
	if [ "${VIM_VERSION:-}" == "" ]
	then
		VIM_VERSION="$MAX_VIM_VERSION"
	fi
	if [ "${GO_VERSION:-}" == "" ]
	then
		GO_VERSION="$MAX_GO_VERSION"
	fi
fi

docker pull govim/govim:${GO_VERSION}_${VIM_FLAVOR}_${VIM_VERSION}_v1

cat Dockerfile.user \
	| GO_VERSION=$GO_VERSION VIM_FLAVOR=$VIM_FLAVOR VIM_VERSION=$VIM_VERSION envsubst '$GO_VERSION,$VIM_FLAVOR,$VIM_VERSION' \
	| docker build -t govim --build-arg USER=$USER --build-arg UID=$UID --build-arg GID=$(id -g $USER) -

#!/usr/bin/env bash
set -u

portless_bin="/home/pal/.local/share/mise/installs/node/26.7.0/bin/portless"

echo "OpenGTM local domain setup"
echo "This will configure https://opengtm.sh through Portless."
echo

"$portless_bin" proxy stop -p 1355 || true

if "$portless_bin" proxy start --tld sh; then
  if "$portless_bin" alias opengtm 3010 && "$portless_bin" hosts sync; then
    echo
    "$portless_bin" doctor
    echo
    echo "OpenGTM is ready at https://opengtm.sh"
    setup_status=0
  else
    echo "Portless started, but route or hosts setup failed."
    setup_status=1
  fi
else
  echo "Setup was not authorized; restoring the :1355 fallback."
  PORTLESS_SYNC_HOSTS=0 "$portless_bin" proxy start -p 1355 --no-tls --tld sh --skip-trust
  PORTLESS_SYNC_HOSTS=0 "$portless_bin" alias opengtm 3010
  setup_status=1
fi

echo
if [ "$setup_status" -eq 0 ]; then
  echo "Success. You can close this window."
else
  echo "Setup did not complete. The fallback remains http://opengtm.sh:1355"
fi
read -r -p "Press Enter to close..."
exit "$setup_status"

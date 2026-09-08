#!/usr/bin/env bash
# Exchange an MFA code for a temporary session, so the day-to-day workflow can
# satisfy the MFA conditions in s3.tf and iam-mfa.tf.
#
#   eval "$(./tools/mfa-session.sh 123456)"
#   make deploy
#
# The point of the eval is that the credentials land in the shell's environment
# and expire on their own. Nothing is written to disk, and nothing outlives the
# duration below.
set -euo pipefail

CODE="${1:-}"
DURATION="${MFA_SESSION_DURATION:-3600}"

if [ -z "$CODE" ]; then
  echo "usage: eval \"\$(./tools/mfa-session.sh <6-digit-code>)\"" >&2
  exit 2
fi

# Unset any existing session so the base long-lived key mints the new one;
# otherwise a stale or expired token here fails in a confusing way.
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN

USER_NAME="$(aws iam get-user --query 'User.UserName' --output text)"
SERIAL="$(aws iam list-mfa-devices --user-name "$USER_NAME" \
            --query 'MFADevices[0].SerialNumber' --output text)"

if [ -z "$SERIAL" ] || [ "$SERIAL" = "None" ]; then
  echo "No MFA device is enrolled on $USER_NAME. Enrol one before enabling enforcement:" >&2
  echo "  aws iam create-virtual-mfa-device --virtual-mfa-device-name $USER_NAME \\" >&2
  echo "      --outfile /tmp/mfa-qr.png --bootstrap-method QRCodePNG" >&2
  echo "  # scan /tmp/mfa-qr.png with your authenticator, then two CONSECUTIVE codes:" >&2
  echo "  aws iam enable-mfa-device --user-name $USER_NAME \\" >&2
  echo "      --serial-number arn:aws:iam::<account>:mfa/$USER_NAME \\" >&2
  echo "      --authentication-code1 <code> --authentication-code2 <next code>" >&2
  echo "  shred -u /tmp/mfa-qr.png   # the QR image IS the secret" >&2
  exit 1
fi

CREDS="$(aws sts get-session-token \
           --serial-number "$SERIAL" \
           --token-code "$CODE" \
           --duration-seconds "$DURATION" \
           --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken,Expiration]' \
           --output text)"

read -r AKID SECRET TOKEN EXPIRES <<<"$CREDS"
echo "export AWS_ACCESS_KEY_ID=$AKID"
echo "export AWS_SECRET_ACCESS_KEY=$SECRET"
echo "export AWS_SESSION_TOKEN=$TOKEN"
echo "# session valid until $EXPIRES" >&2

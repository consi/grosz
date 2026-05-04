// Base64url encoding/decoding for WebAuthn ArrayBuffer ↔ JSON transport.

export function base64urlEncode(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let str = '';
  for (const b of bytes) str += String.fromCharCode(b);
  return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export function base64urlDecode(str: string): ArrayBuffer {
  const pad = str.length % 4;
  const s = str.replace(/-/g, '+').replace(/_/g, '/') + (pad ? '===='.slice(pad) : '');
  const raw = atob(s);
  const buf = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) buf[i] = raw.charCodeAt(i);
  return buf.buffer;
}

/* eslint-disable @typescript-eslint/no-explicit-any */

export function prepareCreationOptions(opts: any): PublicKeyCredentialCreationOptions {
  opts.publicKey.challenge = base64urlDecode(opts.publicKey.challenge);
  opts.publicKey.user.id = base64urlDecode(opts.publicKey.user.id);
  if (opts.publicKey.excludeCredentials) {
    for (const c of opts.publicKey.excludeCredentials) {
      c.id = base64urlDecode(c.id);
    }
  }
  return opts.publicKey;
}

export function prepareRequestOptions(opts: any): PublicKeyCredentialRequestOptions {
  opts.publicKey.challenge = base64urlDecode(opts.publicKey.challenge);
  if (opts.publicKey.allowCredentials) {
    for (const c of opts.publicKey.allowCredentials) {
      c.id = base64urlDecode(c.id);
    }
  }
  return opts.publicKey;
}

export function encodeRegistrationResponse(cred: PublicKeyCredential): object {
  const resp = cred.response as AuthenticatorAttestationResponse;
  const result: any = {
    id: cred.id,
    rawId: base64urlEncode(cred.rawId),
    type: cred.type,
    response: {
      attestationObject: base64urlEncode(resp.attestationObject),
      clientDataJSON: base64urlEncode(resp.clientDataJSON),
    },
  };
  if (typeof resp.getTransports === 'function') {
    result.response.transports = resp.getTransports();
  }
  return result;
}

export function encodeAuthenticationResponse(cred: PublicKeyCredential): object {
  const resp = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: base64urlEncode(cred.rawId),
    type: cred.type,
    response: {
      authenticatorData: base64urlEncode(resp.authenticatorData),
      clientDataJSON: base64urlEncode(resp.clientDataJSON),
      signature: base64urlEncode(resp.signature),
      userHandle: resp.userHandle ? base64urlEncode(resp.userHandle) : '',
    },
  };
}

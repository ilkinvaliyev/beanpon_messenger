# XMPP client auth (JWT) — one-time setup

ejabberd authenticates XMPP clients with `auth_method: jwt`. A client logs in
with a short-lived token that carries its `jid` claim, signed with the SAME
secret as Beanpon's normal JWTs (`JWT_SECRET`). The Go service mints that token
at `POST /api/v1/xmpp/token` from the user's existing JWT.

## One-time: create the JWT key file

ejabberd needs the raw signing secret in a file. Create it from your server
`.env` `JWT_SECRET`, next to the compose file:

```bash
cd xmpp/deploy
# Replace <JWT_SECRET> with the exact value from the messenger service .env
printf '%s' '<JWT_SECRET>' > jwt.key
chmod 600 jwt.key
```

Important:
- Use `printf` (not `echo`) so there is **no trailing newline** — the secret
  must match byte-for-byte what the Go service uses to sign.
- Do NOT commit `jwt.key` (add it to .gitignore).

## Apply

```bash
cd xmpp/deploy
docker compose -f docker-compose.xmpp.yml up -d --force-recreate
docker logs beanpon_ejabberd --tail 40   # expect "started", no auth errors
```

## How the end-to-end flow works

1. iOS already holds a normal Beanpon JWT (Keychain).
2. Before connecting XMPP, iOS calls `POST /api/v1/xmpp/token` with that JWT.
   The Go service returns `{ token, jid, expires_in }` where `token` is a JWT
   containing `jid: "<user_id>@beanpon.com"`, signed with `JWT_SECRET`.
3. iOS connects to `wss://xmpp.beanpon.com/ws` and authenticates via SASL PLAIN
   using that token as the password.
4. ejabberd verifies the token against `jwt.key` and binds the JID. Done.

No separate XMPP password, no change to the auth service, and the JWT remains
the single source of truth.

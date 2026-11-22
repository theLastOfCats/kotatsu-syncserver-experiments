"""Extra surface checks beyond the sync, history-policy and recovery suites."""
import gzip
import http.client
import json


def check(port):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=10)

    def request(method, path, status, *, token=None, payload=None):
        headers = {'Authorization': 'Bearer ' + token} if token else {}
        if payload is not None:
            headers['Content-Type'] = 'application/json'
        conn.request(method, path, json.dumps(payload) if payload is not None else None, headers)
        response = conn.getresponse()
        body = response.read()
        if response.status != status:
            raise ValueError(f'{method} {path}: expected {status}, got {response.status}')
        return body

    def expect(ok, message):
        if not ok:
            raise ValueError(message)

    try:
        expect(request('GET', '/', 200) == b'Alive', 'health body')
        expect(not request('HEAD', '/', 200), 'HEAD returned a body')
        request('GET', '/me', 401)
        email = 'matrix-profile@example.invalid'
        token = json.loads(request('POST', '/auth', 200, payload={
            'email': email, 'password': 'matrix-profile',
        }))['token']
        profile = json.loads(request('GET', '/me', 200, token=token))
        expect(profile['email'] == email and type(profile['id']) is int, 'profile identity')
        # The sync suite uploaded this card. The public catalog must reflect what devices wrote.
        card = json.loads(request('GET', '/manga/900001', 200))
        expect(card['manga_id'] == 900001 and card['title'] == 'A synthetic manga', 'catalog card')
        first = json.loads(request('GET', '/manga?offset=0&limit=1', 200))
        second = json.loads(request('GET', '/manga?offset=1&limit=1', 200))
        expect(len(first) == len(second) == 1 and first[0]['manga_id'] != second[0]['manga_id'], 'catalog paging')
        conn.request('GET', '/manga?offset=0&limit=50', headers={'Accept-Encoding': 'gzip'})
        catalog = conn.getresponse()
        expect(catalog.status == 200 and catalog.getheader('Content-Encoding') == 'gzip', 'catalog gzip negotiation')
        cards = json.loads(gzip.decompress(catalog.read()))
        expect(card in cards, 'compressed catalog omits the uploaded card')
        request('GET', '/manga?offset=0', 400)
        request('GET', '/manga/not-a-number', 404)
        expect(request('GET', '/deeplink/reset-password', 400) == b'Missing token', 'missing deep link token')
        page = request('GET', '/deeplink/reset-password?token=matrix-marker', 200)
        expect(b'kotatsu://reset-password' in page and b'matrix-marker' in page, 'reset deep link')
    finally:
        conn.close()

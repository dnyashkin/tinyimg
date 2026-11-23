package main

// Per-IP лимитер: token bucket (запросы/мин) + лимит одновременных сжатий.
// Защита от злоумышленника, который захочет завалить сервис тяжёлыми
// загрузками.
//
// Ключ бакета — канонический IP. X-Forwarded-For учитывается ТОЛЬКО если
// непосредственный клиент (RemoteAddr) входит в список доверенных прокси
// (-trust-proxy): иначе заголовок подделывается любым клиентом и лимит
// обходится ротацией значений. В списке доверенных берётся последний валидный
// адрес из XFF — его дописал последний доверенный хоп (nginx
// $proxy_add_x_forwarded_for), тогда как первые элементы целиком подконтрольны
// клиенту.

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxBuckets — потолок карты корзин: новые ключи за потолком отклоняются
	// (fail-closed для ротирующего адреса, легитимные корзины не страдают).
	maxBuckets = 8192
	// bucketTTL — сколько корзина живёт без запросов.
	bucketTTL = 10 * time.Minute
)

type ipBucket struct {
	tokens   float64
	last     time.Time
	active   int
	lastSeen time.Time
}

type ipLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*ipBucket
	rate      float64 // токенов в секунду
	burst     float64
	maxConc   int
	interval  time.Duration
	lastSweep time.Time
}

func newIPLimiter(perMinute, burst, maxConcurrent int) *ipLimiter {
	if perMinute <= 0 {
		return nil // лимитер выключен
	}
	if burst <= 0 {
		burst = perMinute
	}
	return &ipLimiter{
		buckets:   make(map[string]*ipBucket),
		rate:      float64(perMinute) / 60,
		burst:     float64(burst),
		maxConc:   maxConcurrent,
		interval:  time.Minute,
		lastSweep: time.Now(),
	}
}

// canonicalIP — нормализует адрес (::ffff:1.2.3.4 → 1.2.3.4) и отсекает
// мусор: ключ бакета всегда фиксированной длины, мусорные строки из
// заголовков в карту не попадают.
func canonicalIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := canonicalIP(host); ip != "" {
		return ip
	}
	return host
}

// ipFromRequest — адрес клиента для бакета. XFF учитывается только за
// доверенным прокси (trusted), иначе берётся адрес соединения.
func ipFromRequest(r *http.Request, trusted []*net.IPNet) string {
	peer := peerIP(r)
	if len(trusted) == 0 {
		return peer
	}
	pip := net.ParseIP(peer)
	if pip == nil {
		return peer
	}
	trustedPeer := false
	for _, n := range trusted {
		if n != nil && n.Contains(pip) {
			trustedPeer = true
			break
		}
	}
	if !trustedPeer {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := canonicalIP(parts[i]); ip != "" {
				return ip
			}
		}
	}
	return peer
}

// bucketKey — ключ корзины. IPv4 — точный адрес; IPv6 — префикс /64:
// у клиента целый /64 под контролем (2⁶⁴ адресов), и поточечный ключ
// позволял бы обходить и токены, и лимит параллелизма, и потолок карты.
func bucketKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip // мусор из RemoteAddr — как есть (канонизирован выше)
	}
	if p4 := p.To4(); p4 != nil {
		return p4.String()
	}
	b := p.To16()
	for i := 8; i < 16; i++ {
		b[i] = 0
	}
	return b.String() + "/64"
}

// allow списывает токен; ok=false → 429, retryAfter — через сколько повторить.
func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	key := bucketKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	b := l.buckets[key]
	if b == nil {
		// Потолок карты — fail-closed для НОВЫХ ключей: ротация адресов
		// получает 429, а легитимные корзины продолжают работать.
		if len(l.buckets) >= maxBuckets {
			return false, retryAfter(l, now)
		}
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.lastSeen = now
	// пополнение
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens += dt * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		retry := time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Millisecond
		return false, retry
	}
	b.tokens--
	return true, 0
}

// retryAfter — пауза, после которой освободится место в карте (bucketTTL).
func retryAfter(l *ipLimiter, now time.Time) time.Duration {
	oldest := now
	for _, b := range l.buckets {
		if b.lastSeen.Before(oldest) {
			oldest = b.lastSeen
		}
	}
	d := bucketTTL - now.Sub(oldest)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// acquire занимает слот одновременного сжатия; false → слишком много параллельных.
func (l *ipLimiter) acquire(ip string) bool {
	if l == nil || l.maxConc <= 0 {
		return true
	}
	key := bucketKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			return false // как в allow: новые ключи за потолком не принимаем
		}
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.lastSeen = now
	if b.active >= l.maxConc {
		return false
	}
	b.active++
	return true
}

func (l *ipLimiter) release(ip string) {
	if l == nil || l.maxConc <= 0 {
		return
	}
	key := bucketKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[key]; b != nil && b.active > 0 {
		b.active--
	}
}

// sweepLocked — уборка протухших корзин не чаще раза в минуту (иначе каждый
// запрос сканировал бы всю карту под глобальным мьютексом). Потолок карты
// соблюдают allow/acquire при создании новых корзин (fail-closed), поэтому
// полный сброс здесь — только страховка на случай будущих правок.
func (l *ipLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.interval {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > bucketTTL && b.active == 0 {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) > maxBuckets {
		l.buckets = make(map[string]*ipBucket)
	}
}

// retryAfterSecs — честный Retry-After по реальному времени ожидания.
func retryAfterSecs(retry time.Duration) int {
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// reject — ответ 429 для обычного API.
func (l *ipLimiter) reject(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSecs(retry)))
	writeJSON(w, http.StatusTooManyRequests, compressResponse{Error: "слишком много запросов, попробуйте позже"})
}

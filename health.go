package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

// El plan gratuito de Render suspende el servicio tras un rato sin trafico, y
// el primer pedido despues de eso paga el arranque en frio: cerca de un minuto
// en el que el front no tiene con quien hablar. /api/health existe para que
// alguien pueda golpear la puerta y saber cuando se abrio, sin arrastrar el
// costo de una consulta de verdad.
var startedAt = time.Now()

// healthTimeout corta el ping a la base. Si Postgres no contesta en dos
// segundos, el problema ya no es el arranque en frio y conviene decirlo.
const healthTimeout = 2 * time.Second

func health(w http.ResponseWriter, r *http.Request) {
	// Sin cache en ningun lado: la respuesta vale por el instante en que se
	// pide. Un intermediario que la guarde convierte el chequeo en adorno.
	w.Header().Set("Cache-Control", "no-store")

	// El servicio esta vivo, pero sin base no puede servir nada util, asi que
	// se lo pregunta. Es un ida y vuelta sobre el pool ya abierto.
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	if err := db.Ping(ctx); err != nil {
		log.Printf("health: la base no responde: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded",
			"db":     false,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"db":     true,
		// Segundos desde el arranque del proceso. Un valor chico en un servicio
		// que deberia llevar horas arriba es la firma de una suspension por
		// inactividad, y sirve para saber si el keep-alive esta funcionando.
		"uptime_s": int(time.Since(startedAt).Seconds()),
	})
}

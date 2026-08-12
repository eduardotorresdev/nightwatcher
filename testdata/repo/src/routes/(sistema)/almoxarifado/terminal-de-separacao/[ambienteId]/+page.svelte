<script lang="ts">
  // Alvo do culprit de http-client-nao-previsto.json (N+1 API Call).
  // Uma requisição por ambiente dentro do laço — exatamente o padrão que o
  // Sentry detecta como N+1.
  let { data } = $props();

  async function carregarSeparacoes(ambientes: { id: string }[]) {
    const listas = [];
    for (const amb of ambientes) {
      const r = await fetch(
        `/api/collections/ambientes_prontos_revisao/records?fields=*&page=1&perPage=50&filter=ambiente="${amb.id}"`,
      );
      listas.push(await r.json());
    }
    return listas;
  }
</script>

<h1>Terminal de separação</h1>

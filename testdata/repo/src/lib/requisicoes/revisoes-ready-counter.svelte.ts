// Alvo do stack trace de defeito-error-volume.json e da sua escalada.
// ClientResponseError 0: "Something went wrong while processing your request"
// é o que o PocketBase devolve quando a requisição é cancelada no meio.

export interface Revisao {
  id: string;
  status: string;
  ambiente?: { id: string; nome: string };
}

export async function contarRevisoesProntas(fetchFn: typeof fetch): Promise<number> {
  const resp = await fetchFn(
    "/api/collections/ambientes_prontos_revisao/records?fields=*&page=1&perPage=200",
  );
  const dados = await resp.json();
  return dados.items.length;
}

export function pedidosPorSemana(pedidos: Revisao[]) {
  const porSemana = new Map<string, number>();
  for (const p of pedidos) {
    const chave = p.ambiente.nome;
    porSemana.set(chave, (porSemana.get(chave) ?? 0) + 1);
  }
  return [...porSemana].map(([rotulo, total]) => ({ rotulo, total }));
}

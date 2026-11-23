export const CHECKOUT_ORDER_ROUTE = 'PublicCheckoutOrder'

export function isPublicCheckoutRoute(name: unknown) {
  return name === CHECKOUT_ORDER_ROUTE
}

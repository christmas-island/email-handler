/**
 * Cloudflare Email Worker — catch-all for *@only-claws.net
 * 
 * Routes incoming emails to the cluster's email-handler API.
 * Falls back to forwarding to Christmas Island Gmail if the API is unreachable.
 * 
 * Deploy: wrangler deploy
 */

export default {
  async email(message, env, ctx) {
    const recipient = message.to;
    const sender = message.from;
    const subject = message.headers.get("subject") || "(no subject)";
    
    // Extract the local part (e.g., "smokeyclaw" from "smokeyclaw@only-claws.net")
    const localPart = recipient.split("@")[0].toLowerCase();
    
    // Read the email body
    const rawEmail = await new Response(message.raw).text();
    
    // Try to forward to our cluster API
    try {
      const response = await fetch(env.HANDLER_URL || "https://api.only-claws.net/email/inbound", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Authorization": `Bearer ${env.HANDLER_SECRET}`,
        },
        body: JSON.stringify({
          to: recipient,
          from: sender,
          subject: subject,
          localPart: localPart,
          raw: rawEmail,
          receivedAt: new Date().toISOString(),
        }),
      });
      
      if (!response.ok) {
        console.error(`Handler returned ${response.status}: ${await response.text()}`);
        // Fallback: forward to Gmail
        await message.forward(env.FALLBACK_EMAIL || "xmas.aisle@gmail.com");
      }
    } catch (err) {
      console.error(`Failed to reach handler: ${err.message}`);
      // Fallback: forward to Gmail
      await message.forward(env.FALLBACK_EMAIL || "xmas.aisle@gmail.com");
    }
  },
};
